package mrwatch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The notifier (slice M5) turns mrwatch state transitions into durable nudges.
// After every poll it diffs the fresh snapshot against the last one it saw and,
// when a merge request enters an unhealthy state worth surfacing, it does two
// independent things:
//
//   - Durable inbox: resolves the MR's source branch to an owning agent via the
//     coordination snapshot and sends a directed agent_message_send so even a
//     future session recalls the stall. An unknown branch yields no inbox
//     message (the attention lane still fires).
//   - Attention lane: records a read-only `merge`/`conflict` lane item for the
//     mobile/HUD attention surface, matching the existing closed lane contract
//     ({agent, namespace, merge, conflict}; classify by TYPE first).
//
// It runs as an additive post-poll hook (Poller.AddPostPoll), independent of
// the M4 shepherd: neither depends on the other, and disabling one leaves the
// other running.
//
// Dedup: an inbox message fires once per MR+state. Re-entering the SAME state
// on a later poll is suppressed; the state must change away and back before the
// notifier re-emits. State that leaves the notify set (goes healthy, draft,
// awaiting, or running) clears the memory so a genuine re-stall re-notifies.
//
// Failure isolation: a send error is logged and swallowed. The notifier must
// never panic or block the poll goroutine — a broken agent-context bridge
// degrades to attention-lane-only, exactly like an unreachable GitLab degrades
// the registry.

// EnvNotify is the M5 kill switch. It gates BOTH the durable inbox sends and
// the attention-lane items. DEFAULT ON: attention lanes are read-only UI, so
// the notifier is enabled unless explicitly turned off with a falsey value
// ("off"/"0"/"false"/"no"/"disable"/"disabled").
const EnvNotify = "LOOM_MRWATCH_NOTIFY"

// notifyMaxAttention bounds how many attention-lane items the notifier exposes,
// so a large fleet of stalled MRs can't unbounded-grow the mobile payload.
const notifyMaxAttention = 12

// notifyStates is the set of unhealthy states a transition INTO which is worth a
// nudge. It is deliberately narrower than "not ok": awaiting_pipeline,
// ci_running and draft_idle are transient/expected and do not notify.
var notifyStates = map[State]struct{}{
	StateConflict:              {},
	StateCIFailedFlaky:         {},
	StateCIFailedDeterministic: {},
	StateAutomergeUnarmed:      {},
	StatePipelineSkipped:       {},
	StateStaleBranch:           {},
}

// isNotifyState reports whether entering state should raise a nudge.
func isNotifyState(state State) bool {
	_, ok := notifyStates[state]
	return ok
}

// Owner identifies the agent/session that owns an MR's source branch, resolved
// from the coordination snapshot. AgentID is the join result; SessionID and
// Namespace enrich the message when present.
type Owner struct {
	AgentID   string
	SessionID string
	Namespace string
}

// OwnerResolver maps an MR source branch to its owning agent. It reports
// ok=false (and a zero Owner) when the branch is not owned by any live agent —
// the notifier then emits no inbox message but still records the attention
// lane. The HUD wires this to the coordination snapshot; tests supply a fake.
type OwnerResolver func(branch string) (Owner, bool)

// InboxMessage is a directed durable message the notifier hands to a sender.
type InboxMessage struct {
	ToAgentID   string
	FromAgentID string
	Subject     string
	Body        string
	SessionRef  string
}

// MessageSender delivers a durable inbox message to an agent. The HUD backs
// this with the agent-context bridge (agent_message_send); tests supply a fake.
// A non-nil error is logged and swallowed — delivery is best-effort.
type MessageSender interface {
	SendInbox(ctx context.Context, msg InboxMessage) error
}

// AttentionItem is one read-only attention-lane entry the notifier exposes for
// the mobile/HUD surface. Lane is the classified lane TYPE ("merge" or
// "conflict") per the closed mobile contract; the mobile domain renders it into
// the wire lane shape. All fields are wire-safe.
type AttentionItem struct {
	Repo     string    `json:"repo"`
	IID      int64     `json:"iid"`
	Branch   string    `json:"branch,omitempty"`
	State    string    `json:"state"`
	Lane     string    `json:"lane"`
	Reason   string    `json:"reason,omitempty"`
	WebURL   string    `json:"web_url,omitempty"`
	Severity string    `json:"severity"`
	AgentID  string    `json:"agent_id,omitempty"`
	Since    time.Time `json:"since"`
}

// Notifier detects mrwatch transitions and drives inbox + attention nudges.
// Safe for concurrent use: Notify runs on the poll goroutine while Attention()
// serves HTTP readers.
type Notifier struct {
	sender   MessageSender
	resolver OwnerResolver
	enabled  bool
	fromID   string
	logger   *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	last   map[string]State         // repo!iid -> last notify-state seen (dedup)
	active map[string]AttentionItem // repo!iid -> current attention lane item
}

// NotifierOptions configures a Notifier. Zero values fall back to defaults.
type NotifierOptions struct {
	// Enabled gates both inbox sends and attention items. Build it from
	// NotifyEnabledFromEnv so the LOOM_MRWATCH_NOTIFY kill switch applies.
	Enabled bool
	// FromAgentID is the sender id stamped on inbox messages. Defaults to
	// "loom-mrwatch" so a recipient can see the nudge came from the watcher.
	FromAgentID string
	Logger      *slog.Logger
	Now         func() time.Time
}

// DefaultNotifierFromID is the sender id stamped on inbox nudges when unset.
const DefaultNotifierFromID = "loom-mrwatch"

// NewNotifier builds a Notifier. A nil sender or nil resolver is tolerated: a
// nil sender disables inbox delivery (attention lanes still populate), a nil
// resolver makes every branch "unowned" (attention-only). Passing enabled=false
// yields an inert notifier whose Notify is a no-op.
func NewNotifier(sender MessageSender, resolver OwnerResolver, opts NotifierOptions) *Notifier {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	fromID := strings.TrimSpace(opts.FromAgentID)
	if fromID == "" {
		fromID = DefaultNotifierFromID
	}
	return &Notifier{
		sender:   sender,
		resolver: resolver,
		enabled:  opts.Enabled,
		fromID:   fromID,
		logger:   logger,
		now:      nowFn,
		last:     make(map[string]State),
		active:   make(map[string]AttentionItem),
	}
}

// NotifyEnabledFromEnv reports whether the notifier is enabled. DEFAULT ON:
// unset or any value other than an explicit falsey token keeps it on.
func NotifyEnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvNotify))) {
	case "off", "0", "false", "no", "disable", "disabled":
		return false
	default:
		return true
	}
}

// Enabled reports whether the notifier will act.
func (n *Notifier) Enabled() bool {
	if n == nil {
		return false
	}
	return n.enabled
}

// Notify is the post-poll hook. It diffs the snapshot against the notifier's
// last-seen state, sends an inbox nudge on each transition into a notify-worthy
// state (deduped), and maintains the current attention-lane set. Safe to call
// on a nil or disabled notifier (no-op).
func (n *Notifier) Notify(ctx context.Context, snap Snapshot) {
	if n == nil || !n.enabled {
		return
	}
	now := n.now()
	live := make(map[string]struct{}, len(snap.MergeRequests))

	for _, mr := range snap.MergeRequests {
		key := stampKey(mr.Repo, mr.IID)
		live[key] = struct{}{}

		if !isNotifyState(mr.State) {
			// Left (or never entered) the notify set: drop dedup memory and any
			// attention item so a genuine re-stall re-notifies later.
			n.clear(key)
			continue
		}

		owner, owned := n.resolveOwner(mr.SourceBranch)

		// Attention lane always reflects current state (read-only UI); refresh
		// it every poll while the MR is unhealthy.
		n.setActive(key, AttentionItem{
			Repo:     mr.Repo,
			IID:      mr.IID,
			Branch:   mr.SourceBranch,
			State:    string(mr.State),
			Lane:     laneFor(mr.State),
			Reason:   mr.Reason,
			WebURL:   mr.WebURL,
			Severity: severityFor(mr.State),
			AgentID:  owner.AgentID,
			Since:    transitionTime(mr, now),
		})

		// Inbox nudge fires only on a real transition into this state (dedup).
		if !n.markTransition(key, mr.State) {
			continue
		}
		if n.sender == nil || !owned || owner.AgentID == "" {
			n.logger.Debug("mrwatch notify: transition without owning agent; attention-only",
				"repo", mr.Repo, "mr_iid", mr.IID, "branch", mr.SourceBranch, "state", mr.State)
			continue
		}
		n.sendInbox(ctx, mr, owner)
	}

	n.prune(live)
}

// sendInbox delivers the durable nudge, logging and swallowing any error so a
// broken bridge can never crash the poll goroutine.
func (n *Notifier) sendInbox(ctx context.Context, mr MergeRequest, owner Owner) {
	body := formatInboxBody(mr)
	msg := InboxMessage{
		ToAgentID:   owner.AgentID,
		FromAgentID: n.fromID,
		Subject:     fmt.Sprintf("MR !%d %s", mr.IID, mr.State),
		Body:        body,
		SessionRef:  owner.SessionID,
	}
	if err := n.sender.SendInbox(ctx, msg); err != nil {
		n.logger.Warn("mrwatch notify: inbox send failed; nudge dropped",
			"repo", mr.Repo, "mr_iid", mr.IID, "to_agent", owner.AgentID,
			"state", mr.State, "error", err.Error())
		return
	}
	n.logger.Info("mrwatch notify: inbox nudge sent",
		"repo", mr.Repo, "mr_iid", mr.IID, "to_agent", owner.AgentID, "state", mr.State)
}

// resolveOwner runs the injected resolver, tolerating a nil resolver.
func (n *Notifier) resolveOwner(branch string) (Owner, bool) {
	if n.resolver == nil || strings.TrimSpace(branch) == "" {
		return Owner{}, false
	}
	return n.resolver(branch)
}

// markTransition records the MR's current notify-state and reports whether this
// is a fresh transition (new MR, or state changed since last poll). Caller-
// serialized via n.mu.
func (n *Notifier) markTransition(key string, state State) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	prev, ok := n.last[key]
	if ok && prev == state {
		return false
	}
	n.last[key] = state
	return true
}

// setActive stores/refreshes the attention item for an unhealthy MR.
func (n *Notifier) setActive(key string, item AttentionItem) {
	n.mu.Lock()
	n.active[key] = item
	n.mu.Unlock()
}

// clear drops the dedup memory and attention item for an MR that is no longer
// in a notify-worthy state.
func (n *Notifier) clear(key string) {
	n.mu.Lock()
	delete(n.last, key)
	delete(n.active, key)
	n.mu.Unlock()
}

// prune drops state for MRs no longer present in the snapshot (merged/closed),
// so the maps can't grow unbounded.
func (n *Notifier) prune(live map[string]struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k := range n.last {
		if _, ok := live[k]; !ok {
			delete(n.last, k)
		}
	}
	for k := range n.active {
		if _, ok := live[k]; !ok {
			delete(n.active, k)
		}
	}
}

// Attention returns a stable-ordered copy of the current attention-lane items,
// capped at notifyMaxAttention. Always non-nil so the caller encodes [] never
// null. A nil/disabled notifier yields an empty slice.
func (n *Notifier) Attention() []AttentionItem {
	if n == nil {
		return []AttentionItem{}
	}
	n.mu.Lock()
	out := make([]AttentionItem, 0, len(n.active))
	for _, item := range n.active {
		out = append(out, item)
	}
	n.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		// Most severe first, then oldest transition, then stable by repo/iid.
		if si, sj := severityRank(out[i].Severity), severityRank(out[j].Severity); si != sj {
			return si > sj
		}
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].IID < out[j].IID
	})
	if len(out) > notifyMaxAttention {
		out = out[:notifyMaxAttention]
	}
	return out
}

// formatInboxBody renders the spec's message body:
//
//	MR !<IID> (<branch>) entered <state>: <reason> — <web_url>
//
// The reason falls back to the state when the classifier left it empty, and the
// trailing " — <url>" is omitted when the MR has no web URL.
func formatInboxBody(mr MergeRequest) string {
	reason := strings.TrimSpace(mr.Reason)
	if reason == "" {
		reason = string(mr.State)
	}
	body := fmt.Sprintf("MR !%d (%s) entered %s: %s", mr.IID, mr.SourceBranch, mr.State, reason)
	if url := strings.TrimSpace(mr.WebURL); url != "" {
		body += " — " + url
	}
	return body
}

// laneFor classifies a notify-state into a mobile attention lane TYPE. Per the
// closed contract ({agent, namespace, merge, conflict}) and the "classify by
// type first" rule, a conflicted MR maps to the dedicated `conflict` lane and
// every other unhealthy merge state maps to the `merge` lane. No new lanes are
// invented.
func laneFor(state State) string {
	if state == StateConflict {
		return "conflict"
	}
	return "merge"
}

// severityFor maps a notify-state to the mobile severity vocabulary
// ("info"/"warning"/"critical") used by the other attention lanes.
func severityFor(state State) string {
	switch state {
	case StateConflict, StateCIFailedDeterministic:
		return "critical"
	case StateCIFailedFlaky, StatePipelineSkipped, StateAutomergeUnarmed:
		return "warning"
	default: // stale_branch and any other notify-state
		return "info"
	}
}

// severityRank orders severities for the attention sort (higher = surface
// first).
func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

// transitionTime is when the MR entered its current state, preferring the
// registry's last_transition_at and falling back to now.
func transitionTime(mr MergeRequest, now time.Time) time.Time {
	if !mr.LastTransitionAt.IsZero() {
		return mr.LastTransitionAt
	}
	return now
}
