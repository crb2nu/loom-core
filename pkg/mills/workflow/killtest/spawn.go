package killtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type spawnConfigMapWire struct {
	Metadata kubernetesObjectMetadataWire `json:"metadata"`
	Data     map[string]string            `json:"data"`
}

type spawnStateWire struct {
	SpawnID string `json:"spawn_id"`
	Status  string `json:"status"`
	Request struct {
		Branch         string `json:"branch"`
		IdempotencyKey string `json:"idempotency_key"`
	} `json:"request"`
}

func isTerminalSpawnStatus(status string) bool {
	switch status {
	case "completed", "failed", "stopped":
		return true
	default:
		return false
	}
}

func parseSpawnStateConfigMap(raw, runID string) (SpawnStateSnapshot, error) {
	return parseSpawnStateConfigMapForNamespace(raw, s1cSpawnNamespace, runID)
}

func parseSpawnStateConfigMapForNamespace(raw, expectedNamespace, runID string) (SpawnStateSnapshot, error) {
	var cm spawnConfigMapWire
	if err := json.Unmarshal([]byte(raw), &cm); err != nil {
		return SpawnStateSnapshot{}, err
	}
	identity := KubernetesObjectIdentity{
		Name: cm.Metadata.Name, Namespace: cm.Metadata.Namespace,
		UID: cm.Metadata.UID, ResourceVersion: cm.Metadata.ResourceVersion,
		Terminating: cm.Metadata.DeletionTimestamp != nil,
	}
	if cm.Metadata.DeletionTimestamp != nil {
		identity.DeletionTimestamp = *cm.Metadata.DeletionTimestamp
	}
	if err := ValidateKubernetesObjectIdentity(identity, expectedNamespace, spawnStateConfigMapName); err != nil {
		return SpawnStateSnapshot{}, fmt.Errorf("durable spawn ConfigMap identity: %w", err)
	}
	wantBranch := ""
	if runID != "" {
		wantBranch = "mills-wf/" + runID
	}
	snapshot := SpawnStateSnapshot{
		ConfigMapUID:      identity.UID,
		ConfigMapIdentity: identity,
		Statuses:          make(map[string]string),
		IdempotencyKeys:   make(map[string]string),
	}
	for key, encoded := range cm.Data {
		var state spawnStateWire
		if err := json.Unmarshal([]byte(encoded), &state); err != nil {
			return SpawnStateSnapshot{}, fmt.Errorf("decode spawn state %s: %w", key, err)
		}
		if wantBranch != "" && state.Request.Branch != wantBranch {
			continue
		}
		if state.SpawnID != "" && state.SpawnID != key {
			return SpawnStateSnapshot{}, fmt.Errorf("spawn state key %q disagrees with payload id %q", key, state.SpawnID)
		}
		id := state.SpawnID
		if id == "" {
			id = key
		}
		snapshot.RecordIDs = append(snapshot.RecordIDs, id)
		snapshot.Statuses[id] = state.Status
		snapshot.IdempotencyKeys[id] = state.Request.IdempotencyKey
		if !isTerminalSpawnStatus(state.Status) {
			snapshot.ActiveIDs = append(snapshot.ActiveIDs, id)
		}
	}
	sort.Strings(snapshot.RecordIDs)
	sort.Strings(snapshot.ActiveIDs)
	return snapshot, nil
}

func (h *Harness) getSpawnStateSnapshot(ctx context.Context, runID string) (SpawnStateSnapshot, error) {
	raw, err := h.kubectl(ctx, "-n", h.cfg.SpawnNS, "get", "configmap", "loom-spawn-state", "-o", "json")
	if err != nil {
		return SpawnStateSnapshot{}, err
	}
	return parseSpawnStateConfigMapForNamespace(raw, h.cfg.SpawnNS, runID)
}

// SpawnRecordStatus reads the durable HUD record for the exact deterministic
// identity. Status=running is published immediately before agent execution;
// pod readiness alone can still mean the orchestrator is configuring an idle
// runtime container.
func (h *Harness) SpawnRecordStatus(ctx context.Context, spawnID string) (string, error) {
	snapshot, err := h.getSpawnStateSnapshot(ctx, "")
	if err != nil {
		return "", err
	}
	status, ok := snapshot.Statuses[spawnID]
	if !ok {
		// The workflow journal is record-before-dispatch, so a short interval
		// before HUD registration is expected. Callers poll this empty status;
		// malformed/unavailable ConfigMaps still return an error above.
		return "", nil
	}
	if status == "" {
		return "", fmt.Errorf("durable spawn record %q has an empty status", spawnID)
	}
	return status, nil
}

type activeSpawnPodListWire struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

func isSpawnRelatedPod(name string, labels map[string]string) bool {
	_, hasSpawnID := labels["loom.dev/spawn-id"]
	return strings.HasPrefix(name, "spawn-") ||
		labels["app.kubernetes.io/managed-by"] == "loom-spawn" ||
		hasSpawnID
}

func parseActivePodNames(raw string) ([]string, error) {
	var pods activeSpawnPodListWire
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		return nil, err
	}
	var names []string
	for _, pod := range pods.Items {
		if !isSpawnRelatedPod(pod.Metadata.Name, pod.Metadata.Labels) {
			continue
		}
		// Only Succeeded/Failed are proven terminal. Unknown, an empty phase,
		// and a deleting pod all remain blockers because a process may still be
		// running while the API object is uncertain or terminating.
		if pod.Status.Phase != "Succeeded" && pod.Status.Phase != "Failed" {
			names = append(names, pod.Metadata.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (h *Harness) activeSpawnPodNames(ctx context.Context) ([]string, error) {
	// List the entire namespace: missing/corrupt labels are themselves a
	// condition the fleet fence must catch on deterministic spawn-* pods.
	raw, err := h.kubectl(ctx, "-n", h.cfg.SpawnNS, "get", "pods", "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseActivePodNames(raw)
}

type spawnPodSample struct {
	Concurrent   int
	ReadyRunning int
	Names        []string
	Incarnations []PodIdentity
}

func parseSpawnPodSample(raw, expectedName string) (spawnPodSample, error) {
	var pods podListWire
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		return spawnPodSample{}, err
	}
	sample := spawnPodSample{}
	for _, pod := range pods.Items {
		if pod.Metadata.Name == "" || pod.Metadata.UID == "" {
			return spawnPodSample{}, fmt.Errorf("spawn pod observation is missing name or uid")
		}
		if pod.Metadata.Name != expectedName {
			return spawnPodSample{}, fmt.Errorf("field-selected spawn pod %q differs from derived name %q", pod.Metadata.Name, expectedName)
		}

		identity := PodIdentity{
			Name:      pod.Metadata.Name,
			UID:       pod.Metadata.UID,
			Node:      pod.Spec.NodeName,
			StartedAt: pod.Status.StartTime,
		}
		if len(pod.Spec.Containers) > 0 {
			identity.Image = pod.Spec.Containers[0].Image
		}
		if len(pod.Status.ContainerStatuses) > 0 {
			identity.ImageID = pod.Status.ContainerStatuses[0].ImageID
		}
		sample.Names = append(sample.Names, identity.Name)
		sample.Incarnations = append(sample.Incarnations, identity)
		if pod.Status.Phase != "Succeeded" && pod.Status.Phase != "Failed" {
			sample.Concurrent++
		}
		if pod.Metadata.DeletionTimestamp == nil && pod.Status.Phase == "Running" &&
			len(pod.Spec.Containers) == 1 && len(pod.Status.ContainerStatuses) == 1 &&
			pod.Status.ContainerStatuses[0].Ready && pod.Status.ContainerStatuses[0].ImageID != "" {
			sample.ReadyRunning++
		}
	}
	sort.Strings(sample.Names)
	sort.Slice(sample.Incarnations, func(i, j int) bool {
		return sample.Incarnations[i].UID < sample.Incarnations[j].UID
	})
	return sample, nil
}

func mergePodIdentity(current, observed PodIdentity) PodIdentity {
	if current.Name == "" {
		current.Name = observed.Name
	}
	if current.UID == "" {
		current.UID = observed.UID
	}
	if current.Node == "" {
		current.Node = observed.Node
	}
	if current.Image == "" {
		current.Image = observed.Image
	}
	if current.ImageID == "" {
		current.ImageID = observed.ImageID
	}
	if current.StartedAt.IsZero() {
		current.StartedAt = observed.StartedAt
	}
	return current
}

func (h *Harness) rememberSpawnPodIncarnations(spawnID string, incarnations []PodIdentity) error {
	h.spawnPodMu.Lock()
	defer h.spawnPodMu.Unlock()
	if h.spawnPodHistory == nil {
		h.spawnPodHistory = make(map[string]map[string]PodIdentity)
	}
	byUID := h.spawnPodHistory[spawnID]
	if byUID == nil {
		byUID = make(map[string]PodIdentity)
		h.spawnPodHistory[spawnID] = byUID
	}
	for _, observed := range incarnations {
		if observed.UID == "" || observed.Name == "" {
			return fmt.Errorf("cannot retain spawn pod incarnation without name and uid")
		}
		if current, ok := byUID[observed.UID]; ok {
			if current.Name != observed.Name {
				return fmt.Errorf("spawn pod uid %q changed name from %q to %q", observed.UID, current.Name, observed.Name)
			}
			byUID[observed.UID] = mergePodIdentity(current, observed)
			continue
		}
		byUID[observed.UID] = observed
	}
	return nil
}

func (h *Harness) observedSpawnPodIncarnations(spawnID string) []PodIdentity {
	h.spawnPodMu.Lock()
	defer h.spawnPodMu.Unlock()
	byUID := h.spawnPodHistory[spawnID]
	incarnations := make([]PodIdentity, 0, len(byUID))
	for _, identity := range byUID {
		incarnations = append(incarnations, identity)
	}
	sort.Slice(incarnations, func(i, j int) bool {
		return incarnations[i].UID < incarnations[j].UID
	})
	return incarnations
}

func mergeSpawnPodIncarnations(ev *Evidence, observed []PodIdentity) {
	byUID := make(map[string]int, len(ev.TotalSpawnPodIncarnations))
	for i, identity := range ev.TotalSpawnPodIncarnations {
		byUID[identity.UID] = i
	}
	for _, identity := range observed {
		if i, ok := byUID[identity.UID]; ok {
			ev.TotalSpawnPodIncarnations[i] = mergePodIdentity(ev.TotalSpawnPodIncarnations[i], identity)
			continue
		}
		byUID[identity.UID] = len(ev.TotalSpawnPodIncarnations)
		ev.TotalSpawnPodIncarnations = append(ev.TotalSpawnPodIncarnations, identity)
	}
	sort.Slice(ev.TotalSpawnPodIncarnations, func(i, j int) bool {
		return ev.TotalSpawnPodIncarnations[i].UID < ev.TotalSpawnPodIncarnations[j].UID
	})
}

// CountSpawnPods returns (concurrent, names) for the derived pod name
// spawn-<spawnID> in the spawn namespace. "concurrent" counts pods in a
// non-terminal phase. Every observation also retains the exact pod UID and
// immutable attribution so AwaitTerminal can fold the pre-crash and terminal
// samples into Evidence without changing this method's public API.
func (h *Harness) CountSpawnPods(ctx context.Context, spawnID string) (int, []string, error) {
	active, _, names, err := h.SpawnPodStatus(ctx, spawnID)
	return active, names, err
}

// SpawnPodStatus distinguishes existence from a non-terminating Running+Ready
// workload. The kill window is meaningful only after exactly one ready pod is
// executing; Pending/ImagePullBackOff objects still count as active blockers.
func (h *Harness) SpawnPodStatus(ctx context.Context, spawnID string) (active, ready int, names []string, err error) {
	raw, err := h.kubectl(ctx, "-n", h.cfg.SpawnNS, "get", "pods",
		"--field-selector", "metadata.name=spawn-"+spawnID,
		"-o", "json")
	if err != nil {
		return 0, 0, nil, err
	}
	sample, err := parseSpawnPodSample(raw, "spawn-"+spawnID)
	if err != nil {
		return 0, 0, nil, err
	}
	if err := h.rememberSpawnPodIncarnations(spawnID, sample.Incarnations); err != nil {
		return 0, 0, nil, err
	}
	return sample.Concurrent, sample.ReadyRunning, sample.Names, nil
}

// SpawnPodTornDown reports whether the exact spawn pod is past process
// observation: deleted, phase-terminal, terminating (deletionTimestamp set),
// or with every container terminated. Kubelet destroys the container sandbox
// before the pod object clears its deletion grace period, so exec-based
// process probes become impossible while the pod is still listable.
func (h *Harness) SpawnPodTornDown(ctx context.Context, spawnID string) (bool, error) {
	raw, err := h.kubectl(ctx, "-n", h.cfg.SpawnNS, "get", "pods",
		"--field-selector", "metadata.name=spawn-"+spawnID,
		"-o", "json")
	if err != nil {
		return false, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					State struct {
						Terminated *struct {
							ExitCode int `json:"exitCode"`
						} `json:"terminated"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return false, fmt.Errorf("parse spawn pod teardown state: %w", err)
	}
	for _, item := range list.Items {
		if item.Metadata.DeletionTimestamp != nil {
			continue
		}
		if item.Status.Phase == "Succeeded" || item.Status.Phase == "Failed" {
			continue
		}
		allTerminated := len(item.Status.ContainerStatuses) > 0
		for _, cs := range item.Status.ContainerStatuses {
			if cs.State.Terminated == nil {
				allTerminated = false
				break
			}
		}
		if allTerminated {
			continue
		}
		return false, nil
	}
	return true, nil
}

func mergeSpawnSnapshot(ev *Evidence, snapshot SpawnStateSnapshot) {
	if ev.FinalSpawnRecordStatuses == nil {
		ev.FinalSpawnRecordStatuses = make(map[string]string)
	}
	if ev.FinalSpawnIdempotencyKeys == nil {
		ev.FinalSpawnIdempotencyKeys = make(map[string]string)
	}
	for _, id := range snapshot.RecordIDs {
		if !contains(ev.TotalSpawnRecordIDs, id) {
			ev.TotalSpawnRecordIDs = append(ev.TotalSpawnRecordIDs, id)
		}
	}
	for id, status := range snapshot.Statuses {
		ev.FinalSpawnRecordStatuses[id] = status
	}
	for id, key := range snapshot.IdempotencyKeys {
		ev.FinalSpawnIdempotencyKeys[id] = key
	}
	sort.Strings(ev.TotalSpawnRecordIDs)
	ev.FinalActiveSpawnRecordIDs = append(ev.FinalActiveSpawnRecordIDs[:0], snapshot.ActiveIDs...)
}

// CaptureSpawnState folds every post-preflight mobile-hud record into evidence.
// Comparing against the baseline catches a drifted duplicate even when its
// request metadata was reconstructed incompletely after a crash.
func (h *Harness) CaptureSpawnState(ctx context.Context, ev *Evidence) error {
	snapshot, err := h.getSpawnStateSnapshot(ctx, "")
	if err != nil {
		return err
	}
	filtered := SpawnStateSnapshot{Statuses: make(map[string]string), IdempotencyKeys: make(map[string]string)}
	for _, id := range snapshot.RecordIDs {
		if contains(ev.BaselineSpawnRecordIDs, id) {
			continue
		}
		filtered.RecordIDs = append(filtered.RecordIDs, id)
		filtered.Statuses[id] = snapshot.Statuses[id]
		filtered.IdempotencyKeys[id] = snapshot.IdempotencyKeys[id]
		if contains(snapshot.ActiveIDs, id) {
			filtered.ActiveIDs = append(filtered.ActiveIDs, id)
		}
	}
	mergeSpawnSnapshot(ev, filtered)
	return nil
}
