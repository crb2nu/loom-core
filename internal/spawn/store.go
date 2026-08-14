package spawn

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// ErrSpawnStateConflict means a durable keyed spawn transition lost to a
// previously committed owner/terminal result. Callers must not emit lifecycle
// side effects for the rejected transition; they should reload the winner.
var ErrSpawnStateConflict = stderrors.New("spawn state conflict")

// ErrConfigMapSizeBudgetExceeded identifies a spawn-state write that would
// exceed the configured serialized ConfigMap safety budget. It is intentionally
// distinct from an API-server rejection so callers can surface capacity and
// prune retained terminal rows before retrying.
var ErrConfigMapSizeBudgetExceeded = stderrors.New("spawn state ConfigMap size budget exceeded")

// ConfigMapSizeBudgetError describes the exact candidate rejected before an
// oversized ConfigMap write reached the Kubernetes API.
type ConfigMapSizeBudgetError struct {
	Namespace       string
	Name            string
	Operation       string
	SerializedBytes int
	BudgetBytes     int
}

func (e *ConfigMapSizeBudgetError) Error() string {
	return fmt.Sprintf(
		"%s %s/%s would serialize to %d bytes, exceeding the %d-byte spawn-state safety budget; prune retained terminal rows or select a larger state store",
		e.Operation, e.Namespace, e.Name, e.SerializedBytes, e.BudgetBytes,
	)
}

func (e *ConfigMapSizeBudgetError) Unwrap() error {
	return ErrConfigMapSizeBudgetExceeded
}

// Store persists spawn state for recovery after restart.
type Store interface {
	Save(ctx context.Context, state *State) error
	Load(ctx context.Context, id string) (*State, error)
	LoadAll(ctx context.Context) ([]*State, error)
	Delete(ctx context.Context, id string) error
}

// DeleteCondition identifies one immutable durable spawn generation. Empty
// values are exact values, never wildcards.
type DeleteCondition struct {
	DriverOwnerID  string
	IdempotencyKey string
	StartedAt      time.Time
}

// ConditionalDeleteStore is implemented by shared multi-writer stores. It
// prevents a stale controller from deleting a newer peer replacement that
// reused the deterministic spawn ID.
type ConditionalDeleteStore interface {
	DeleteIfMatch(ctx context.Context, id string, condition DeleteCondition) (deleted bool, err error)
}

// PressurePrunableStore is implemented by stores that can discard terminal
// history when their serialized representation approaches its capacity limit.
// Returned states are the exact durable generations removed.
type PressurePrunableStore interface {
	PruneTerminalToSoftLimit(ctx context.Context) ([]*State, error)
}

// ---------- FileStore ----------

// FileStore persists spawn state as JSON files on disk, providing backward
// compatibility with the original HUD persistence layer.
type FileStore struct {
	dir string
}

// NewFileStore creates a FileStore, ensuring the directory exists.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("spawn store directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create spawn store dir: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// Save persists a spawn state to disk as <spawn_id>.json.
func (s *FileStore) Save(_ context.Context, state *State) error {
	if state == nil {
		return fmt.Errorf("cannot save nil spawn state")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spawn state: %w", err)
	}
	path := s.path(state.SpawnID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write spawn state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename spawn state: %w", err)
	}
	return nil
}

// Load reads a single spawn state by ID.
func (s *FileStore) Load(_ context.Context, id string) (*State, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read spawn state %s: %w", id, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("unmarshal spawn state %s: %w", id, err)
	}
	return &st, nil
}

// LoadAll reads all persisted spawn states from disk.
func (s *FileStore) LoadAll(_ context.Context) ([]*State, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read spawn store dir: %w", err)
	}
	var states []*State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil {
			continue
		}
		states = append(states, &st)
	}
	return states, nil
}

// Delete removes the persisted state file for a spawn.
func (s *FileStore) Delete(_ context.Context, id string) error {
	path := s.path(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete spawn state %s: %w", id, err)
	}
	return nil
}

// PruneCompleted removes state files for terminal spawns older than maxAge.
func (s *FileStore) PruneCompleted(ctx context.Context, maxAge time.Duration) error {
	states, err := s.LoadAll(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, st := range states {
		if !IsTerminal(st.Status) {
			continue
		}
		if st.EndedAt != nil && st.EndedAt.Before(cutoff) {
			if err := s.Delete(ctx, st.SpawnID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *FileStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// DefaultStoreDir returns the default file store directory.
func DefaultStoreDir() string {
	if cfgDir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(cfgDir, "loom", "spawns")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "loom", "spawns")
}

// ---------- K8sConfigMapStore ----------

// K8sConfigMapStore stores spawn state as data entries in a Kubernetes
// ConfigMap, providing cluster-native persistence that survives node-local
// filesystem loss.
type K8sConfigMapStore struct {
	client             kubernetes.Interface
	namespace          string
	name               string // ConfigMap name
	maxSerializedBytes int
}

// DefaultK8sConfigMapSerializedSizeBudget leaves 128 KiB of headroom below
// Kubernetes' 1 MiB ConfigMap data ceiling for object metadata, managed fields,
// JSON keys/escaping, and serializer differences between this guard and the API
// server.
const DefaultK8sConfigMapSerializedSizeBudget = 896 * 1024

// configMapSoftPruneNumerator/Denominator retain terminal history during
// normal operation but leave enough headroom for new spawn records under load.
const (
	configMapSoftPruneNumerator   = 4
	configMapSoftPruneDenominator = 5
)

// K8sConfigMapStoreOption customizes a ConfigMap-backed spawn store.
type K8sConfigMapStoreOption func(*K8sConfigMapStore)

// WithK8sConfigMapSerializedSizeBudget overrides the conservative default.
// Non-positive values are ignored so the safety guard cannot be disabled by an
// invalid configuration.
func WithK8sConfigMapSerializedSizeBudget(maxBytes int) K8sConfigMapStoreOption {
	return func(store *K8sConfigMapStore) {
		if maxBytes > 0 {
			store.maxSerializedBytes = maxBytes
		}
	}
}

// NewK8sConfigMapStore creates a ConfigMap-backed store.
func NewK8sConfigMapStore(
	client kubernetes.Interface,
	namespace, name string,
	opts ...K8sConfigMapStoreOption,
) *K8sConfigMapStore {
	if name == "" {
		name = "loom-spawn-state"
	}
	store := &K8sConfigMapStore{
		client:             client,
		namespace:          namespace,
		name:               name,
		maxSerializedBytes: DefaultK8sConfigMapSerializedSizeBudget,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	return store
}

// Save persists a spawn state as a ConfigMap data entry keyed by spawn ID.
func (s *K8sConfigMapStore) Save(ctx context.Context, state *State) error {
	if state == nil {
		return fmt.Errorf("cannot save nil spawn state")
	}
	incomingData, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal spawn state: %w", err)
	}

	var mergeErr error
	err = s.mutateCM(ctx, configMapMutationSave, func(entries map[string]string) bool {
		mergeErr = nil
		existingData, exists := entries[state.SpawnID]
		if !exists {
			entries[state.SpawnID] = string(incomingData)
			return true
		}
		// An Update may have committed even when its HTTP response was lost.
		// Exact replay is the same writer retrying the same transition, not a
		// second winner, so acknowledge it without another mutation.
		if existingData == string(incomingData) {
			return false
		}

		merged, write, err := mergeK8sSpawnState(existingData, state)
		if err != nil {
			mergeErr = err
			return false
		}
		if !write {
			return false
		}
		data, err := json.Marshal(merged)
		if err != nil {
			mergeErr = fmt.Errorf("marshal merged spawn state %s: %w", state.SpawnID, err)
			return false
		}
		if existingData == string(data) {
			return false
		}
		entries[state.SpawnID] = string(data)
		return true
	})
	if err != nil {
		return err
	}
	return mergeErr
}

// mergeK8sSpawnState protects one ConfigMap entry from logically stale
// same-spawn writers. ConfigMap resourceVersion retries prevent whole-object
// lost updates, but without this per-entry merge a second HUD can still replace
// a complete keyed row with label-only discovery state. Unkeyed rows retain
// legacy last-writer-wins semantics; keyed rows gain immutable request identity,
// sticky stop/cleanup intent, and a first-terminal-winner rule.
func mergeK8sSpawnState(existingData string, incoming *State) (*State, bool, error) {
	var existing State
	if err := json.Unmarshal([]byte(existingData), &existing); err != nil {
		return nil, false, fmt.Errorf("decode existing spawn state %s before update: %w", incoming.SpawnID, err)
	}

	// Driver ownership is established before dispatch and is immutable. The
	// sole migration exception is an ownerless legacy row being claimed by the
	// configured recovery authority. Save cannot identify the authority itself;
	// the controller decides who may attempt this one-way transition, while the
	// ConfigMap resourceVersion retry ensures only one claimant wins.
	existingOwner := strings.TrimSpace(existing.DriverOwnerID)
	incomingOwner := strings.TrimSpace(incoming.DriverOwnerID)
	ownerClaimed := existingOwner == "" && incomingOwner != ""
	if existingOwner != "" && incomingOwner != existingOwner {
		return nil, false, fmt.Errorf(
			"%w for %s: driver owner %q cannot be replaced by %q",
			ErrSpawnStateConflict, incoming.SpawnID, existingOwner, incomingOwner,
		)
	}
	if ownerClaimed {
		// Claim the latest durable bytes, not the claimant's LoadAll snapshot.
		// A legacy writer may have advanced the row between read and claim; the
		// ownership transition must never regress or otherwise rewrite lifecycle
		// state as a side effect.
		claimed := existing
		claimed.DriverOwnerID = incomingOwner
		return &claimed, true, nil
	}

	existingKey := existing.Request.IdempotencyKey
	incomingKey := incoming.Request.IdempotencyKey
	switch {
	case existingKey == "":
		// A complete keyed writer may repair an older label-only row. Two
		// legacy unkeyed writers keep the historical last-writer-wins behavior.
		copy := *incoming
		return &copy, true, nil
	case incomingKey == "":
		// A label-only observer cannot prove ownership of a keyed spawn. Ignore
		// it without error so an older controller cannot corrupt the durable row.
		return nil, false, nil
	case existingKey != incomingKey:
		return nil, false, fmt.Errorf(
			"%w for %s: idempotency key %q cannot replace %q",
			ErrSpawnStateConflict,
			incoming.SpawnID, incomingKey, existingKey,
		)
	}
	// StartedAt is the immutable generation token for a deterministic keyed
	// spawn. Reject rather than merge a stale snapshot: otherwise a cleanup
	// hook can validate generation A, lose an intervening delete/recreate race,
	// then stamp CleanupAt (or repair Error) onto generation B with the same
	// owner and idempotency key.
	if !existing.StartedAt.Equal(incoming.StartedAt) {
		return nil, false, fmt.Errorf(
			"%w for %s: started_at generation changed from %s to %s",
			ErrSpawnStateConflict,
			incoming.SpawnID,
			existing.StartedAt.Format(time.RFC3339Nano),
			incoming.StartedAt.Format(time.RFC3339Nano),
		)
	}

	merged := *incoming
	if existing.SpawnID != "" {
		merged.SpawnID = existing.SpawnID
	}
	if existingOwner != "" {
		merged.DriverOwnerID = existingOwner
	}
	// The request and start identity are established by record-before-dispatch
	// and never change for a logical keyed spawn.
	merged.Request = existing.Request
	if !existing.StartedAt.IsZero() {
		merged.StartedAt = existing.StartedAt
	}
	if existing.AgentID != "" {
		merged.AgentID = existing.AgentID
	}
	if existing.SessionID != "" {
		merged.SessionID = existing.SessionID
	}
	if existing.AuthMode != "" {
		merged.AuthMode = existing.AuthMode
	}
	if existing.PodName != "" && merged.PodName == "" {
		merged.PodName = existing.PodName
	}
	if existing.Telemetry != nil && merged.Telemetry == nil {
		merged.Telemetry = existing.Telemetry
	}
	if existing.CleanupAt != nil {
		merged.CleanupAt = existing.CleanupAt
	}

	// StopRequestedAt is a durable ownership fence. Once set, stale lifecycle
	// writers that did not observe it cannot advance the record; the only status
	// change it permits is the cleanup owner's valid stopped transition.
	if existing.StopRequestedAt != nil {
		if incoming.StopRequestedAt == nil {
			return nil, false, fmt.Errorf(
				"%w for %s: stale writer would erase stop intent",
				ErrSpawnStateConflict,
				incoming.SpawnID,
			)
		}
		if !incoming.StopRequestedAt.Equal(*existing.StopRequestedAt) {
			return nil, false, fmt.Errorf(
				"%w for %s: stop intent was already claimed at %s",
				ErrSpawnStateConflict, incoming.SpawnID, existing.StopRequestedAt.Format(time.RFC3339Nano),
			)
		}
		if incoming.Status != existing.Status && incoming.Status != StatusStopped {
			return nil, false, fmt.Errorf(
				"%w for %s: status %q cannot supersede stopping status %q",
				ErrSpawnStateConflict,
				incoming.SpawnID, incoming.Status, existing.Status,
			)
		}
		merged.StopRequestedAt = existing.StopRequestedAt
	}

	// The first durable terminal result wins. A same-result write may enrich
	// cleanup/telemetry fields, but cannot rewrite the outcome or its timestamp.
	if IsTerminal(existing.Status) {
		if incoming.Status != existing.Status {
			return nil, false, fmt.Errorf(
				"%w for %s: terminal status %q cannot be replaced by %q",
				ErrSpawnStateConflict,
				incoming.SpawnID, existing.Status, incoming.Status,
			)
		}
		cleanupAdded := existing.CleanupAt == nil && incoming.CleanupAt != nil
		errorRepaired := (existing.Status == StatusCompleted || existing.Status == StatusStopped) &&
			existing.Error != "" && incoming.Error == ""
		if !cleanupAdded && !errorRepaired {
			return nil, false, fmt.Errorf(
				"%w for %s: terminal status %q was already committed",
				ErrSpawnStateConflict, incoming.SpawnID, existing.Status,
			)
		}
		merged.Status = existing.Status
		if existing.EndedAt != nil {
			merged.EndedAt = existing.EndedAt
		}
		switch existing.Status {
		case StatusCompleted, StatusStopped:
			merged.Error = ""
		case StatusFailed:
			if existing.Error != "" {
				merged.Error = existing.Error
			}
		}
		if existing.PodName != "" {
			merged.PodName = existing.PodName
		}
		if existing.Telemetry != nil {
			merged.Telemetry = existing.Telemetry
		}
	}

	return &merged, true, nil
}

// Load reads a single spawn state by ID from the ConfigMap.
func (s *K8sConfigMapStore) Load(ctx context.Context, id string) (*State, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get configmap %s: %w", s.name, err)
	}
	raw, ok := cm.Data[id]
	if !ok {
		return nil, nil
	}
	var st State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("unmarshal spawn state %s: %w", id, err)
	}
	return &st, nil
}

// LoadAll reads all spawn states from the ConfigMap.
func (s *K8sConfigMapStore) LoadAll(ctx context.Context) ([]*State, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get configmap %s: %w", s.name, err)
	}
	var states []*State
	for _, raw := range cm.Data {
		var st State
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			continue
		}
		states = append(states, &st)
	}
	return states, nil
}

// Delete removes a spawn state entry from the ConfigMap.
func (s *K8sConfigMapStore) Delete(ctx context.Context, id string) error {
	return s.mutateCM(ctx, configMapMutationDelete, func(entries map[string]string) bool {
		if _, ok := entries[id]; !ok {
			return false
		}
		delete(entries, id)
		return true
	})
}

// PruneTerminalToSoftLimit removes terminal ConfigMap entries oldest-first
// when the serialized ConfigMap exceeds 80% of its safety budget. Unlike
// retention pruning, age is deliberately ignored: this is an overflow valve
// that protects dispatch capacity during high-volume periods. Non-terminal
// entries (and malformed entries, whose lifecycle cannot be safely known) are
// never removed.
func (s *K8sConfigMapStore) PruneTerminalToSoftLimit(ctx context.Context) ([]*State, error) {
	softLimit := s.maxSerializedBytes * configMapSoftPruneNumerator / configMapSoftPruneDenominator
	var pruned []*State
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pruned = nil
		cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		next := cm.DeepCopy()
		serialized, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("serialize ConfigMap %s/%s for pressure prune: %w", s.namespace, s.name, err)
		}
		if len(serialized) <= softLimit {
			return nil
		}

		pruned, err = pruneTerminalEntriesOldestFirst(next, softLimit, nil)
		if err != nil {
			return fmt.Errorf("serialize ConfigMap %s/%s for pressure prune: %w", s.namespace, s.name, err)
		}
		if len(pruned) == 0 {
			return nil
		}
		_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, next, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, err
	}
	return pruned, nil
}

// pruneTerminalEntriesOldestFirst deletes terminal entries from next.Data —
// oldest first, by cleanup/ended/started time — until the serialized ConfigMap
// fits targetBytes or no terminal candidates remain. Keys in exclude are never
// pruned (the Save path passes the entry it is writing, so a final terminal
// state write can't evict itself). Non-terminal and malformed entries are
// never removed. Returns the pruned states; the caller owns persisting next.
func pruneTerminalEntriesOldestFirst(
	next *corev1.ConfigMap,
	targetBytes int,
	exclude map[string]bool,
) ([]*State, error) {
	type candidate struct {
		id    string
		state *State
		at    time.Time
	}
	var candidates []candidate
	for id, raw := range next.Data {
		if exclude[id] {
			continue
		}
		var state State
		if err := json.Unmarshal([]byte(raw), &state); err != nil || !IsTerminal(state.Status) {
			continue
		}
		at := state.StartedAt
		if state.CleanupAt != nil && !state.CleanupAt.IsZero() {
			at = *state.CleanupAt
		}
		if state.EndedAt != nil && !state.EndedAt.IsZero() {
			at = *state.EndedAt
		}
		state.SpawnID = id
		candidates = append(candidates, candidate{id: id, state: &state, at: at})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].at.Equal(candidates[j].at) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].at.Before(candidates[j].at)
	})

	var pruned []*State
	for _, candidate := range candidates {
		delete(next.Data, candidate.id)
		serialized, err := json.Marshal(next)
		if err != nil {
			return pruned, err
		}
		pruned = append(pruned, candidate.state)
		if len(serialized) <= targetBytes {
			break
		}
	}
	return pruned, nil
}

// DeleteIfMatch removes only the exact durable generation observed by the
// caller. The comparison is performed inside every ConfigMap resourceVersion
// retry, closing the Load-then-Delete race.
func (s *K8sConfigMapStore) DeleteIfMatch(
	ctx context.Context,
	id string,
	condition DeleteCondition,
) (bool, error) {
	var (
		deleted  bool
		matchErr error
	)
	err := s.mutateCM(ctx, configMapMutationDelete, func(entries map[string]string) bool {
		deleted = false
		matchErr = nil
		raw, ok := entries[id]
		if !ok {
			return false
		}
		var current State
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			matchErr = fmt.Errorf("decode spawn state %s before conditional delete: %w", id, err)
			return false
		}
		if current.DriverOwnerID != condition.DriverOwnerID ||
			current.Request.IdempotencyKey != condition.IdempotencyKey ||
			!current.StartedAt.Equal(condition.StartedAt) {
			matchErr = fmt.Errorf(
				"%w for %s: durable generation changed before delete",
				ErrSpawnStateConflict, id,
			)
			return false
		}
		delete(entries, id)
		deleted = true
		return true
	})
	if err != nil {
		return false, err
	}
	return deleted, matchErr
}

type configMapMutationMode struct {
	operation              string
	createIfMissing        bool
	allowOversizedDecrease bool
	// pruneTerminalOnOverflow makes an over-budget write self-heal instead of
	// failing: when the size guard rejects the candidate, terminal entries are
	// pruned oldest-first (never the entries this mutation touched) down to
	// the soft limit, then the write proceeds. Without this, a Save that trips
	// the budget is refused outright and the caller fails the spawn create —
	// the periodic PruneTerminalToSoftLimit loop is the only relief valve, and
	// a burst between its ticks 400s every dispatch ("persist owned spawn
	// before dispatch", 18 of 73 failed stage-attempts on 2026-07-26).
	pruneTerminalOnOverflow bool
}

var (
	configMapMutationSave = configMapMutationMode{
		operation:               "save spawn state to ConfigMap",
		createIfMissing:         true,
		pruneTerminalOnOverflow: true,
	}
	configMapMutationDelete = configMapMutationMode{
		operation:              "delete spawn state from ConfigMap",
		allowOversizedDecrease: true,
	}
)

// mutateCM performs a fresh read/modify/update on every conflict retry. This
// preserves entries written by other HUD replicas instead of replaying a stale
// whole-ConfigMap snapshot over them. Missing ConfigMaps are created only for
// Save; Delete remains a no-op when the shared object does not exist. The size
// check runs inside every retry against the fresh winner. Deletion is allowed
// to write a still-over-budget object only when it strictly shrinks an already
// oversized ConfigMap, permitting repeated prune operations to recover it.
func (s *K8sConfigMapStore) mutateCM(
	ctx context.Context,
	mode configMapMutationMode,
	mutate func(map[string]string) bool,
) error {
	mutateOnce := func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				if !mode.createIfMissing {
					return nil
				}
				initial := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      s.name,
						Namespace: s.namespace,
						Labels: map[string]string{
							ManagedByLabel: ManagedByValue,
						},
					},
					Data: make(map[string]string),
				}
				if !mutate(initial.Data) {
					return nil
				}
				if err := s.validateConfigMapSize(mode, nil, initial); err != nil {
					return err
				}
				if _, createErr := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, initial, metav1.CreateOptions{}); createErr == nil {
					return nil
				} else if !apierrors.IsAlreadyExists(createErr) {
					return fmt.Errorf("create configmap %s: %w", s.name, createErr)
				}

				// Another writer won the create race. Re-read its complete object and
				// merge this mutation through the same optimistic Update path below.
				cm, err = s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
			}
			if err != nil {
				return err
			}

			next := cm.DeepCopy()
			if next.Data == nil {
				next.Data = make(map[string]string)
			}
			if !mutate(next.Data) {
				return nil
			}
			if err := s.validateConfigMapSize(mode, cm, next); err != nil {
				var budgetErr *ConfigMapSizeBudgetError
				if !mode.pruneTerminalOnOverflow || !stderrors.As(err, &budgetErr) {
					return err
				}
				// Overflow valve, inline: make room by pruning terminal
				// entries this mutation did NOT touch, down to the soft limit
				// so the write also restores burst headroom rather than
				// leaving the ConfigMap pinned at 100% of budget.
				touched := make(map[string]bool)
				for id, raw := range next.Data {
					if prev, ok := cm.Data[id]; !ok || prev != raw {
						touched[id] = true
					}
				}
				softLimit := s.maxSerializedBytes * configMapSoftPruneNumerator / configMapSoftPruneDenominator
				if _, pruneErr := pruneTerminalEntriesOldestFirst(next, softLimit, touched); pruneErr != nil {
					return fmt.Errorf("serialize ConfigMap %s/%s for overflow prune: %w", s.namespace, s.name, pruneErr)
				}
				if err := s.validateConfigMapSize(mode, cm, next); err != nil {
					return err
				}
			}
			_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, next, metav1.UpdateOptions{})
			if apierrors.IsForbidden(err) {
				return fmt.Errorf("configmap %s in namespace %s requires update permission; grant update on configmaps to the HUD service account: %w",
					s.name, s.namespace, err)
			}
			return err
		})
	}

	const maxTransientAttempts = 8
	delay := 25 * time.Millisecond
	for attempt := 1; attempt <= maxTransientAttempts; attempt++ {
		err := mutateOnce()
		if err == nil {
			return nil
		}
		if attempt == maxTransientAttempts || !isTransientK8sStoreError(err) {
			return fmt.Errorf("update configmap %s: %w", s.name, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("update configmap %s: %w", s.name, ctx.Err())
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
	return nil
}

func (s *K8sConfigMapStore) validateConfigMapSize(
	mode configMapMutationMode,
	current, candidate *corev1.ConfigMap,
) error {
	candidateData, err := json.Marshal(candidate)
	if err != nil {
		return fmt.Errorf("serialize ConfigMap %s/%s for size guard: %w", s.namespace, s.name, err)
	}
	candidateSize := len(candidateData)
	if candidateSize <= s.maxSerializedBytes {
		return nil
	}
	if mode.allowOversizedDecrease && current != nil {
		currentData, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("serialize current ConfigMap %s/%s for size guard: %w", s.namespace, s.name, err)
		}
		if candidateSize < len(currentData) {
			return nil
		}
	}
	return &ConfigMapSizeBudgetError{
		Namespace:       s.namespace,
		Name:            s.name,
		Operation:       mode.operation,
		SerializedBytes: candidateSize,
		BudgetBytes:     s.maxSerializedBytes,
	}
}

func isTransientK8sStoreError(err error) bool {
	if apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err) {
		return true
	}
	if stderrors.Is(err, io.EOF) || stderrors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if stderrors.As(err, &netErr) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection reset") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "client connection lost")
}
