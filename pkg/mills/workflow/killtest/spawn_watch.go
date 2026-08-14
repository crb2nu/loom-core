package killtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	maxSpawnPodBaselineItems   int64 = 5000
	maxSpawnPodWatchCandidates int   = 64
	maxSpawnPodWatchEvents     int   = 256
)

type observedSpawnIDLabel struct {
	present bool
	value   string
}

// SpawnPodObserver maintains a gap-free Kubernetes resourceVersion stream.
// Full gates start a namespace-wide observer before launch and bind it to the
// derived spawn identity later. Recovery-only attached runs retain the legacy
// exact-name observer because they are explicitly non-gating.
type SpawnPodObserver struct {
	h             *Harness
	startedAt     time.Time
	initialRV     string
	selector      string
	namespaceWide bool
	cancel        context.CancelFunc
	done          chan struct{}

	mu               sync.Mutex
	spawnID          string
	expectedName     string
	baselineSpawnUID map[string]struct{}
	candidates       map[string]PodIdentity
	candidateLabels  map[string]observedSpawnIDLabel
	events           []SpawnPodWatchEvent
	semanticErr      error
	runErr           error
	runComplete      bool
	stopping         bool

	stopOnce sync.Once
	endedAt  time.Time
	stopErr  error
}

func podIdentityFromWatch(pod *corev1.Pod, expectedName string) (PodIdentity, error) {
	if pod == nil {
		return PodIdentity{}, errors.New("spawn pod watch returned nil pod")
	}
	if pod.Name == "" || pod.UID == "" {
		return PodIdentity{}, fmt.Errorf("spawn pod watch returned incomplete identity name=%q uid=%q", pod.Name, pod.UID)
	}
	if expectedName != "" && pod.Name != expectedName {
		return PodIdentity{}, fmt.Errorf("spawn pod watch returned name=%q uid=%q, want name=%q",
			pod.Name, pod.UID, expectedName)
	}
	identity := PodIdentity{Name: pod.Name, UID: string(pod.UID), Node: pod.Spec.NodeName}
	if pod.Status.StartTime != nil {
		identity.StartedAt = pod.Status.StartTime.Time
	}
	if len(pod.Spec.Containers) > 0 {
		identity.Image = pod.Spec.Containers[0].Image
	}
	if len(pod.Status.ContainerStatuses) > 0 {
		identity.ImageID = pod.Status.ContainerStatuses[0].ImageID
	}
	return identity, nil
}

func mergeWatchedPodIdentity(current, observed PodIdentity) (PodIdentity, error) {
	if current.Name != "" && observed.Name != "" && current.Name != observed.Name {
		return PodIdentity{}, fmt.Errorf("name changed from %q to %q", current.Name, observed.Name)
	}
	if current.UID != "" && observed.UID != "" && current.UID != observed.UID {
		return PodIdentity{}, fmt.Errorf("uid changed from %q to %q", current.UID, observed.UID)
	}
	for label, values := range map[string][2]string{
		"node": {current.Node, observed.Node}, "image": {current.Image, observed.Image},
		"imageID": {current.ImageID, observed.ImageID},
	} {
		if values[0] != "" && values[1] != "" && values[0] != values[1] {
			return PodIdentity{}, fmt.Errorf("%s changed from %q to %q", label, values[0], values[1])
		}
	}
	if !current.StartedAt.IsZero() && !observed.StartedAt.IsZero() && !current.StartedAt.Equal(observed.StartedAt) {
		return PodIdentity{}, fmt.Errorf("start time changed from %s to %s", current.StartedAt, observed.StartedAt)
	}
	return mergePodIdentity(current, observed), nil
}

func (o *SpawnPodObserver) recordSemanticFailure(err error) error {
	if err == nil {
		return nil
	}
	o.mu.Lock()
	if o.semanticErr == nil {
		o.semanticErr = err
	}
	stored := o.semanticErr
	o.mu.Unlock()
	return stored
}

func (o *SpawnPodObserver) observePod(pod *corev1.Pod, eventType string) error {
	expectedName := ""
	if !o.namespaceWide {
		expectedName = o.expectedName
	}
	identity, err := podIdentityFromWatch(pod, expectedName)
	if err != nil {
		return o.recordSemanticFailure(err)
	}

	o.mu.Lock()
	if o.namespaceWide {
		if _, baseline := o.baselineSpawnUID[identity.UID]; baseline {
			o.mu.Unlock()
			return nil
		}
		if !isSpawnRelatedPod(pod.Name, pod.Labels) {
			o.mu.Unlock()
			return nil
		}
	}
	labelValue, labelPresent := pod.Labels["loom.dev/spawn-id"]
	var labelEvidence *string
	if labelPresent {
		labelCopy := labelValue
		labelEvidence = &labelCopy
	}
	if len(o.events) >= maxSpawnPodWatchEvents {
		o.mu.Unlock()
		return o.recordSemanticFailure(fmt.Errorf(
			"spawn pod watch exceeded bounded event limit %d", maxSpawnPodWatchEvents))
	}
	o.events = append(o.events, SpawnPodWatchEvent{
		Type: eventType, ResourceVersion: pod.ResourceVersion, ObservedAt: time.Now().UTC(),
		Pod: identity, SpawnIDLabel: labelEvidence,
	})
	if current, ok := o.candidates[identity.UID]; ok {
		identity, err = mergeWatchedPodIdentity(current, identity)
		if err != nil {
			o.mu.Unlock()
			return o.recordSemanticFailure(fmt.Errorf(
				"spawn pod watch uid %q identity conflict: %w", current.UID, err))
		}
	} else if len(o.candidates) >= maxSpawnPodWatchCandidates {
		o.mu.Unlock()
		return o.recordSemanticFailure(fmt.Errorf(
			"spawn pod watch exceeded bounded candidate limit %d", maxSpawnPodWatchCandidates))
	}
	o.candidates[identity.UID] = identity
	if labelPresent {
		if currentLabel, ok := o.candidateLabels[identity.UID]; ok && currentLabel.present && currentLabel.value != labelValue {
			o.mu.Unlock()
			return o.recordSemanticFailure(fmt.Errorf(
				"spawn pod watch uid %q spawn-id label changed from %q to %q",
				identity.UID, currentLabel.value, labelValue))
		}
		o.candidateLabels[identity.UID] = observedSpawnIDLabel{present: true, value: labelValue}
	}
	observedLabel := o.candidateLabels[identity.UID]
	spawnID := o.spawnID
	boundName := o.expectedName
	candidateCount := len(o.candidates)
	o.mu.Unlock()

	if o.namespaceWide && candidateCount > 1 {
		return o.recordSemanticFailure(fmt.Errorf(
			"namespace spawn pod watch observed multiple post-baseline UIDs (%d)", candidateCount))
	}
	if boundName != "" && identity.Name != boundName {
		return o.recordSemanticFailure(fmt.Errorf(
			"post-baseline spawn-like pod %q uid=%q differs from derived pod name %q",
			identity.Name, identity.UID, boundName))
	}
	if spawnID != "" && observedLabel.present && observedLabel.value != spawnID {
		return o.recordSemanticFailure(fmt.Errorf(
			"post-baseline spawn pod %q uid=%q carries spawn-id label %q, want %q",
			identity.Name, identity.UID, observedLabel.value, spawnID))
	}
	if spawnID != "" {
		if err := o.h.rememberSpawnPodIncarnations(spawnID, []PodIdentity{identity}); err != nil {
			return o.recordSemanticFailure(err)
		}
	}
	return nil
}

func (h *Harness) startSpawnPodObservation(
	ctx context.Context,
	spawnID string,
	namespaceWide bool,
) (*SpawnPodObserver, error) {
	client, err := h.kubernetesClient()
	if err != nil {
		return nil, err
	}
	selector := ""
	expectedName := ""
	if !namespaceWide {
		if strings.TrimSpace(spawnID) == "" {
			return nil, errors.New("exact spawn pod observation requires a spawn id")
		}
		expectedName = "spawn-" + spawnID
		selector = fields.OneTermEqualSelector("metadata.name", expectedName).String()
	}

	watchCtx, cancel := context.WithCancel(ctx)
	pods := client.CoreV1().Pods(h.cfg.SpawnNS)
	list, err := pods.List(watchCtx, metav1.ListOptions{
		FieldSelector: selector,
		Limit:         maxSpawnPodBaselineItems,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("list spawn pods before watch: %w", err)
	}
	if strings.TrimSpace(list.Continue) != "" {
		cancel()
		return nil, fmt.Errorf(
			"spawn pod baseline list is incomplete at bounded item limit %d (continuation token returned)",
			maxSpawnPodBaselineItems,
		)
	}
	if int64(len(list.Items)) > maxSpawnPodBaselineItems {
		cancel()
		return nil, fmt.Errorf(
			"spawn pod baseline list returned %d items, exceeding bounded item limit %d",
			len(list.Items), maxSpawnPodBaselineItems,
		)
	}
	if list.ResourceVersion == "" {
		cancel()
		return nil, errors.New("spawn pod list returned an empty resourceVersion")
	}
	observer := &SpawnPodObserver{
		h: h, initialRV: list.ResourceVersion, selector: selector, namespaceWide: namespaceWide,
		spawnID: spawnID, expectedName: expectedName,
		baselineSpawnUID: make(map[string]struct{}), candidates: make(map[string]PodIdentity),
		candidateLabels: make(map[string]observedSpawnIDLabel),
		cancel:          cancel, done: make(chan struct{}),
	}
	if namespaceWide {
		for i := range list.Items {
			pod := &list.Items[i]
			if !isSpawnRelatedPod(pod.Name, pod.Labels) {
				continue
			}
			identity, identityErr := podIdentityFromWatch(pod, "")
			if identityErr != nil {
				cancel()
				return nil, fmt.Errorf("validate namespace spawn baseline: %w", identityErr)
			}
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				cancel()
				return nil, fmt.Errorf(
					"namespace watch handshake found active spawn-related pod %q uid=%q phase=%q",
					pod.Name, pod.UID, pod.Status.Phase)
			}
			observer.baselineSpawnUID[identity.UID] = struct{}{}
		}
	} else {
		for i := range list.Items {
			if err := observer.observePod(&list.Items[i], "LIST"); err != nil {
				cancel()
				return nil, err
			}
		}
	}

	stream, err := pods.Watch(watchCtx, metav1.ListOptions{
		FieldSelector:       selector,
		ResourceVersion:     list.ResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start spawn pod watch at rv %s: %w", list.ResourceVersion, err)
	}
	observer.startedAt = time.Now().UTC()
	go func() {
		runErr := observer.run(watchCtx, list.ResourceVersion, stream)
		observer.mu.Lock()
		if runErr == nil {
			runErr = observer.semanticErr
		}
		if runErr == nil && !observer.stopping {
			runErr = errors.New("spawn pod watch ended before explicit terminal stop")
		}
		observer.runErr = runErr
		observer.runComplete = true
		observer.mu.Unlock()
		close(observer.done)
	}()
	return observer, nil
}

// StartSpawnNamespaceObservation begins a namespace-wide List+Watch before a
// full gate launch. The baseline may contain terminal historical spawn pods,
// but any active spawn-related pod fails the handshake closed. Every
// post-baseline spawn-like Added/Modified/Deleted event is retained until the
// stream is bound to the canonical derived identity.
func (h *Harness) StartSpawnNamespaceObservation(ctx context.Context) (*SpawnPodObserver, error) {
	return h.startSpawnPodObservation(ctx, "", true)
}

// StartSpawnPodObservation starts the exact-name observer used by non-gating
// recovery/attach runs, whose spawn identity already exists.
func (h *Harness) StartSpawnPodObservation(ctx context.Context, spawnID string) (*SpawnPodObserver, error) {
	return h.startSpawnPodObservation(ctx, spawnID, false)
}

func (o *SpawnPodObserver) run(
	ctx context.Context,
	resourceVersion string,
	stream watch.Interface,
) error {
	client, err := o.h.kubernetesClient()
	if err != nil {
		stream.Stop()
		return err
	}
	for {
		reconnect := false
		for !reconnect {
			select {
			case <-ctx.Done():
				stream.Stop()
				return nil
			case event, ok := <-stream.ResultChan():
				if !ok {
					stream.Stop()
					reconnect = true
					continue
				}
				switch event.Type {
				case watch.Error:
					stream.Stop()
					return fmt.Errorf("spawn pod watch error at rv %s: %w", resourceVersion, apierrors.FromObject(event.Object))
				case watch.Bookmark:
					accessor, accessorErr := meta.Accessor(event.Object)
					if accessorErr != nil {
						stream.Stop()
						return fmt.Errorf("invalid spawn pod watch bookmark: %w", accessorErr)
					}
					if accessor.GetResourceVersion() == "" {
						stream.Stop()
						return errors.New("spawn pod watch bookmark has an empty resourceVersion")
					}
					resourceVersion = accessor.GetResourceVersion()
				case watch.Added, watch.Modified, watch.Deleted:
					pod, ok := event.Object.(*corev1.Pod)
					if !ok {
						stream.Stop()
						return fmt.Errorf("spawn pod watch returned %T, want *v1.Pod", event.Object)
					}
					if pod.ResourceVersion == "" {
						stream.Stop()
						return errors.New("spawn pod watch event has an empty resourceVersion")
					}
					if err := o.observePod(pod, string(event.Type)); err != nil {
						stream.Stop()
						return err
					}
					resourceVersion = pod.ResourceVersion
				default:
					stream.Stop()
					return fmt.Errorf("unexpected spawn pod watch event type %q", event.Type)
				}
			}
		}

		// Normal API watch timeouts are reconnectable without a gap because the
		// next stream resumes from the last consumed resourceVersion.
		for {
			if ctx.Err() != nil {
				return errors.New("spawn pod watch stopped while its stream was disconnected")
			}
			stream, err = client.CoreV1().Pods(o.h.cfg.SpawnNS).Watch(ctx, metav1.ListOptions{
				FieldSelector: o.selector, ResourceVersion: resourceVersion, AllowWatchBookmarks: true,
			})
			if err == nil {
				break
			}
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
				return fmt.Errorf("spawn pod watch resourceVersion %s expired: %w", resourceVersion, err)
			}
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return fmt.Errorf("spawn pod watch could not reconnect from rv %s before stop: %w", resourceVersion, err)
			case <-timer.C:
			}
		}
	}
}

// BindSpawnIdentity binds every event observed since the pre-launch baseline
// to the canonical deterministic spawn name. An alternate name or more than
// one UID fails closed and cancels the stream.
func (o *SpawnPodObserver) BindSpawnIdentity(spawnID string) error {
	if o == nil {
		return errors.New("nil spawn pod observer")
	}
	if strings.TrimSpace(spawnID) == "" {
		return errors.New("bind spawn observation: spawn id is required")
	}
	expectedName := "spawn-" + spawnID
	o.mu.Lock()
	if o.spawnID != "" && o.spawnID != spawnID {
		alreadyBound := o.spawnID
		o.mu.Unlock()
		return o.recordSemanticFailure(fmt.Errorf(
			"spawn pod observer already bound to %q, cannot bind %q", alreadyBound, spawnID))
	}
	o.spawnID = spawnID
	o.expectedName = expectedName
	candidates := make([]PodIdentity, 0, len(o.candidates))
	labels := make(map[string]observedSpawnIDLabel, len(o.candidateLabels))
	for _, identity := range o.candidates {
		candidates = append(candidates, identity)
	}
	for uid, label := range o.candidateLabels {
		labels[uid] = label
	}
	existingErr := o.semanticErr
	o.mu.Unlock()

	bindErr := existingErr
	if bindErr == nil && len(candidates) > 1 {
		bindErr = fmt.Errorf("namespace spawn pod watch observed multiple post-baseline UIDs (%d)", len(candidates))
	}
	if bindErr == nil {
		for _, identity := range candidates {
			if identity.Name != expectedName {
				bindErr = fmt.Errorf(
					"post-baseline spawn-like pod %q uid=%q differs from derived pod name %q",
					identity.Name, identity.UID, expectedName)
				break
			}
			if label := labels[identity.UID]; label.present && label.value != spawnID {
				bindErr = fmt.Errorf(
					"post-baseline spawn pod %q uid=%q carries spawn-id label %q, want %q",
					identity.Name, identity.UID, label.value, spawnID)
				break
			}
		}
	}
	if bindErr == nil && len(candidates) > 0 {
		bindErr = o.h.rememberSpawnPodIncarnations(spawnID, candidates)
	}
	if bindErr != nil {
		bindErr = o.recordSemanticFailure(bindErr)
		o.cancel()
		return bindErr
	}
	return o.AssertHealthy()
}

// AssertHealthy exposes asynchronous watch/reconnect failures before a
// destructive DELETE is authorized.
func (o *SpawnPodObserver) AssertHealthy() error {
	if o == nil {
		return errors.New("nil spawn pod observer")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.semanticErr != nil {
		return o.semanticErr
	}
	if o.runComplete {
		if o.runErr != nil {
			return o.runErr
		}
		return errors.New("spawn pod watch ended before terminal evidence collection")
	}
	return nil
}

func (o *SpawnPodObserver) candidateSnapshot() []PodIdentity {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]PodIdentity, 0, len(o.candidates))
	for _, identity := range o.candidates {
		result = append(result, identity)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result
}

func (o *SpawnPodObserver) eventSnapshot() []SpawnPodWatchEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]SpawnPodWatchEvent, len(o.events))
	copy(result, o.events)
	return result
}

func mergeSpawnPodNames(ev *Evidence, identities []PodIdentity) {
	for _, identity := range identities {
		if identity.Name != "" && !contains(ev.TotalSpawnPodNames, identity.Name) {
			ev.TotalSpawnPodNames = append(ev.TotalSpawnPodNames, identity.Name)
		}
	}
	sort.Strings(ev.TotalSpawnPodNames)
}

// Stop closes the watch only after terminal evidence collection. It folds all
// event-observed identities, including alternate names and Deleted events,
// into Evidence and records whether coverage remained continuous.
func (o *SpawnPodObserver) Stop(ev *Evidence) error {
	if o == nil {
		return errors.New("nil spawn pod observer")
	}
	o.stopOnce.Do(func() {
		o.mu.Lock()
		o.stopping = true
		o.mu.Unlock()
		o.cancel()
		<-o.done
		o.mu.Lock()
		o.stopErr = o.runErr
		spawnID := o.spawnID
		o.mu.Unlock()
		o.endedAt = time.Now().UTC()

		if ev != nil {
			candidates := o.candidateSnapshot()
			ev.SpawnPodWatchEvents = o.eventSnapshot()
			observed := append([]PodIdentity(nil), candidates...)
			if spawnID != "" {
				observed = append(observed, o.h.observedSpawnPodIncarnations(spawnID)...)
			}
			mergeSpawnPodIncarnations(ev, observed)
			mergeSpawnPodNames(ev, observed)
		}
	})
	if ev != nil {
		ev.SpawnPodWatchStartedAt = o.startedAt
		ev.SpawnPodWatchEndedAt = o.endedAt
		ev.SpawnPodWatchInitialRV = o.initialRV
		ev.SpawnPodWatchContinuous = o.stopErr == nil
		if o.stopErr != nil {
			appendObservationError(ev, "continuous spawn pod watch: "+o.stopErr.Error())
		}
	}
	return o.stopErr
}

// RecordStart checkpoints the List+Watch handshake. Continuous remains false
// until Stop confirms a clean terminal span.
func (o *SpawnPodObserver) RecordStart(ev *Evidence) {
	if o == nil || ev == nil {
		return
	}
	ev.SpawnPodWatchStartedAt = o.startedAt
	ev.SpawnPodWatchInitialRV = o.initialRV
	ev.SpawnPodWatchContinuous = false
}
