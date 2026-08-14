package mrwatch

import (
	"context"
	"log/slog"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Source lists open merge requests for a single GitLab project path. The real
// implementation wraps the mills GitLab client (see source_gitlab.go); tests
// supply a fake so no network or token is required.
type Source interface {
	// ListOpenMRs returns every open MR for the given project path (e.g.
	// "services/loom-core"). It must return a non-nil error on failure so the
	// poller can degrade gracefully; a nil error with an empty slice means
	// "polled fine, no open MRs".
	ListOpenMRs(ctx context.Context, project string) ([]MRInfo, error)
}

// MergedLister is an optional Source capability: it lists the merge requests of
// a project that merged at or after `since`, so the registry can publish an
// explicit merged marker instead of letting a merged MR vanish. A Source that
// does not implement it contributes no merged entries of its own; retention
// still applies to any merged MR that arrives via ListOpenMRs.
//
// Server-side listing (rather than inferring a merge from a disappearance) is
// what makes the marker survive a restart or a HUD outage: the merges that
// happened while the daemon was down are still within the window on the next
// successful poll.
type MergedLister interface {
	ListMergedMRs(ctx context.Context, project string, since time.Time) ([]MRInfo, error)
}

// Poller periodically refreshes the MR registry for a set of projects. It is
// safe for concurrent use; the HUD REST layer reads Snapshot() while the poll
// loop writes.
type Poller struct {
	src             Source
	projects        []string
	interval        time.Duration
	staleAfter      time.Duration
	mergedRetention time.Duration
	logger          *slog.Logger
	now             func() time.Time
	rand            *rand.Rand

	mu     sync.RWMutex
	snap   Snapshot
	byPrj  map[string][]MergeRequest // last good MRs per project (degraded-mode retention)
	stamp  map[string]stateStamp     // per-MR state + since (last_transition_at)
	merged map[string]MergeRequest   // retained merged MRs, expired by mergedRetention

	// postPolls are invoked, in registration order, with the freshly-rebuilt
	// snapshot at the end of every poll (clean or degraded). The M4 shepherd and
	// the M5 notifier each register one hook here so they run on the same cadence
	// as the poll while staying independent of each other. Hooks are called on
	// the poll goroutine, holding no poller lock; a slow hook delays the next
	// poll but can never deadlock a snapshot read.
	postPolls []func(context.Context, Snapshot)
}

// stateStamp records the last classified state of an MR and when it entered it,
// so last_transition_at only moves on an actual state change.
type stateStamp struct {
	state State
	since time.Time
}

// Options configures a Poller. Zero values fall back to sensible defaults.
type Options struct {
	Projects   []string
	Interval   time.Duration
	StaleAfter time.Duration
	// MergedRetention is how long a merged MR stays in the snapshot carrying an
	// explicit StateMerged. Zero uses DefaultMergedRetention; a NEGATIVE value
	// disables retention entirely, restoring the pre-marker behavior where a
	// merged MR is dropped on sight. (Zero cannot mean "off" here: every other
	// Options field treats zero as "use the default".)
	MergedRetention time.Duration
	Logger          *slog.Logger
	// Now and Rand are injectable for deterministic tests. Nil uses time.Now
	// and a time-seeded PRNG.
	Now  func() time.Time
	Rand *rand.Rand
}

// DefaultInterval is the poll cadence when unset. The spec calls for a
// 60–120s jittered interval; 90s is the midpoint.
const DefaultInterval = 90 * time.Second

// minInterval floors the configured interval so a misconfiguration can't
// hammer the GitLab API.
const minInterval = 20 * time.Second

// DefaultMergedRetention is how long a merged MR is kept in the registry when
// unset. Long enough that a consumer polling a few times a day still observes
// the merge, short enough that the retained set stays small.
const DefaultMergedRetention = 72 * time.Hour

// maxRetainedMerged caps the retained merged set regardless of the window, so a
// repo merging faster than the window can never grow the registry without
// bound. The newest merges are kept.
const maxRetainedMerged = 200

// NewPoller builds a Poller over the given Source. src must be non-nil.
func NewPoller(src Source, opts Options) *Poller {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if interval < minInterval {
		interval = minInterval
	}
	staleAfter := opts.StaleAfter
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	mergedRetention := opts.MergedRetention
	if mergedRetention == 0 {
		mergedRetention = DefaultMergedRetention
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	rng := opts.Rand
	if rng == nil {
		// #nosec G404 -- poll-interval jitter only; not security-sensitive
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	projects := normalizeProjects(opts.Projects)

	return &Poller{
		src:             src,
		projects:        projects,
		interval:        interval,
		staleAfter:      staleAfter,
		mergedRetention: mergedRetention,
		logger:          logger,
		now:             nowFn,
		rand:            rng,
		snap:            emptySnapshot(projects),
		byPrj:           make(map[string][]MergeRequest, len(projects)),
		stamp:           make(map[string]stateStamp),
		merged:          make(map[string]MergeRequest),
	}
}

// SetPostPoll replaces the registered post-poll hooks with the single hook fn,
// invoked with the fresh snapshot after every poll. Passing nil clears all
// hooks. A nil Poller is a no-op so callers can guard on config. Set it before
// Start so no poll is missed. Prefer AddPostPoll when composing independent
// consumers (shepherd + notifier) so neither clobbers the other.
func (p *Poller) SetPostPoll(fn func(context.Context, Snapshot)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if fn == nil {
		p.postPolls = nil
	} else {
		p.postPolls = []func(context.Context, Snapshot){fn}
	}
	p.mu.Unlock()
}

// AddPostPoll appends a post-poll hook, preserving any already registered. This
// is the additive wiring the M5 notifier uses so it runs alongside — not
// instead of — the M4 shepherd. A nil Poller or nil fn is a no-op. Register
// hooks before Start so no poll is missed.
func (p *Poller) AddPostPoll(fn func(context.Context, Snapshot)) {
	if p == nil || fn == nil {
		return
	}
	p.mu.Lock()
	p.postPolls = append(p.postPolls, fn)
	p.mu.Unlock()
}

// Projects returns the watched project paths (copy).
func (p *Poller) Projects() []string {
	out := make([]string, len(p.projects))
	copy(out, p.projects)
	return out
}

// Start launches the poll loop in a goroutine. It runs one poll immediately,
// then re-polls on a jittered interval until ctx is cancelled. Start returns
// immediately; a nil Poller is a no-op so callers can guard on config.
func (p *Poller) Start(ctx context.Context) {
	if p == nil {
		return
	}
	go p.loop(ctx)
}

func (p *Poller) loop(ctx context.Context) {
	p.pollOnce(ctx)
	for {
		timer := time.NewTimer(p.jitteredInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			p.pollOnce(ctx)
		}
	}
}

// jitteredInterval returns interval + a random [0, interval/2) offset so a
// fleet of daemons doesn't align their GitLab polls.
func (p *Poller) jitteredInterval() time.Duration {
	if p.interval <= 0 {
		return DefaultInterval
	}
	jitter := time.Duration(p.rand.Int63n(int64(p.interval / 2)))
	return p.interval + jitter
}

// pollOnce refreshes every project once and rebuilds the snapshot. A project
// that errors retains its previous MRs and marks the snapshot stale; the poll
// never panics or blocks HUD serving — an unreachable GitLab degrades to the
// last good snapshot.
func (p *Poller) pollOnce(ctx context.Context) {
	now := p.now()
	degraded := false

	for _, project := range p.projects {
		infos, err := p.src.ListOpenMRs(ctx, project)
		if err != nil {
			degraded = true
			p.logger.Warn("mrwatch: list open MRs failed; retaining last snapshot",
				"project", project, "error", err.Error())
			continue // keep prior byPrj[project]
		}
		// Copy before extending: the source may hand back a slice it still owns.
		combined := make([]MRInfo, 0, len(infos))
		combined = append(combined, infos...)
		combined = append(combined, p.listMerged(ctx, project, now)...)
		p.setProjectMRs(project, combined, now)
	}

	p.rebuildSnapshot(now, degraded)

	// Fire the post-poll hooks (M4 shepherd, M5 notifier) with the fresh
	// snapshot. Copy the hook slice under the lock, then call outside it so a
	// slow hook can't block snapshot readers. Hooks run in registration order
	// and share the same snapshot value.
	p.mu.RLock()
	hooks := make([]func(context.Context, Snapshot), len(p.postPolls))
	copy(hooks, p.postPolls)
	p.mu.RUnlock()
	if len(hooks) > 0 {
		snap := p.Snapshot()
		for _, hook := range hooks {
			hook(ctx, snap)
		}
	}
}

// listMerged fetches the project's recently-merged MRs when the source supports
// it and retention is enabled.
//
// A failure here does NOT mark the snapshot degraded: absence of a merged marker
// is already fail-closed for consumers (they simply do not advance), whereas
// flipping Stale would misreport otherwise-fresh open-MR data as retained.
func (p *Poller) listMerged(ctx context.Context, project string, now time.Time) []MRInfo {
	if p.mergedRetention <= 0 {
		return nil
	}
	lister, ok := p.src.(MergedLister)
	if !ok {
		return nil
	}
	infos, err := lister.ListMergedMRs(ctx, project, now.Add(-p.mergedRetention))
	if err != nil {
		p.logger.Warn("mrwatch: list merged MRs failed; merged markers may lag",
			"project", project, "error", err.Error())
		return nil
	}
	return infos
}

// setProjectMRs classifies the fresh MRInfos for one project and stores them.
// Open MRs replace the project's list; merged MRs go to the bounded retention
// set (they are project-independent there, keyed per MR); closed and
// unclassifiable MRs are dropped.
// Must be called under no lock; it takes p.mu internally for the stamp map.
func (p *Poller) setProjectMRs(project string, infos []MRInfo, now time.Time) {
	out := make([]MergeRequest, 0, len(infos))
	for _, info := range infos {
		if info.Repo == "" {
			info.Repo = project
		}
		state, reason := Classify(info, now, p.staleAfter)
		switch state {
		case "":
			continue // lifecycle we cannot name (locked/unknown); drop
		case StateClosed:
			continue // closed WITHOUT merging — never retained, never merged
		}
		since := p.transitionSince(info.Repo, info.IID, state, now)
		mr := MergeRequest{
			Repo:             info.Repo,
			IID:              info.IID,
			Title:            info.Title,
			SourceBranch:     info.SourceBranch,
			TargetBranch:     info.TargetBranch,
			SHA:              info.SHA,
			State:            state,
			Reason:           reason,
			WebURL:           info.WebURL,
			CreatedAt:        info.CreatedAt,
			LastTransitionAt: since,
			Stale:            !info.UpdatedAt.IsZero() && now.Sub(info.UpdatedAt) > p.staleAfter,
		}
		if info.Pipeline != nil {
			mr.PipelineStatus = info.Pipeline.Status
			mr.PipelineURL = info.Pipeline.WebURL
			mr.PipelineID = info.Pipeline.ID
		}
		if state == StateMerged {
			mr.Merged = true
			mr.MergedAt = info.MergedAt
			// A merged MR is terminal: it is stale-by-age only in the sense that
			// nobody will touch the branch again, which is not an actionable
			// stall, so never flag it.
			mr.Stale = false
			p.retainMerged(mr)
			continue
		}
		out = append(out, mr)
	}

	p.mu.Lock()
	p.byPrj[project] = out
	p.mu.Unlock()
}

// retainMerged stores a merged MR in the retention set, keyed like the
// transition stamps so re-observing the same merge replaces rather than
// duplicates it. Expiry and the count cap are applied in rebuildSnapshot.
func (p *Poller) retainMerged(mr MergeRequest) {
	p.mu.Lock()
	p.merged[stampKey(mr.Repo, mr.IID)] = mr
	p.mu.Unlock()
}

// transitionSince returns the timestamp the given MR entered its current state.
// It updates the internal stamp map on a state change.
func (p *Poller) transitionSince(repo string, iid int64, state State, now time.Time) time.Time {
	key := stampKey(repo, iid)
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, ok := p.stamp[key]
	if !ok || prev.state != state {
		p.stamp[key] = stateStamp{state: state, since: now}
		return now
	}
	return prev.since
}

// rebuildSnapshot flattens per-project MRs plus the retained merged set into a
// fresh snapshot with counts. LastPollAt only advances on a clean poll (no
// degraded projects); a degraded poll retains the prior LastPollAt and sets
// Stale=true.
func (p *Poller) rebuildSnapshot(now time.Time, degraded bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	retained := p.sweepMergedLocked(now)
	mergedKeys := make(map[string]struct{}, len(retained))
	for _, mr := range retained {
		mergedKeys[stampKey(mr.Repo, mr.IID)] = struct{}{}
	}

	all := make([]MergeRequest, 0)
	for _, project := range p.projects {
		for _, mr := range p.byPrj[project] {
			// A merge is terminal: if this MR is also in the retained set, the
			// open view is a stale read from the same (or an earlier) poll and
			// must not shadow the merged marker.
			if _, ok := mergedKeys[stampKey(mr.Repo, mr.IID)]; ok {
				continue
			}
			all = append(all, mr)
		}
	}
	all = append(all, retained...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Repo != all[j].Repo {
			return all[i].Repo < all[j].Repo
		}
		return all[i].IID < all[j].IID
	})

	counts := make(map[string]int, len(AllStates()))
	for _, s := range AllStates() {
		counts[string(s)] = 0
	}
	for _, mr := range all {
		counts[string(mr.State)]++
	}

	// Prune stamps for MRs no longer present so the map can't grow unbounded.
	live := make(map[string]struct{}, len(all))
	for _, mr := range all {
		live[stampKey(mr.Repo, mr.IID)] = struct{}{}
	}
	for k := range p.stamp {
		if _, ok := live[k]; !ok {
			delete(p.stamp, k)
		}
	}

	lastPoll := p.snap.LastPollAt
	if !degraded {
		lastPoll = now
	}

	projects := make([]string, len(p.projects))
	copy(projects, p.projects)

	p.snap = Snapshot{
		MergeRequests: all,
		Counts:        counts,
		LastPollAt:    lastPoll,
		Stale:         degraded,
		Projects:      projects,
	}
}

// sweepMergedLocked expires and caps the retained merged set and returns what
// still belongs in the snapshot. Retention is bounded two independent ways —
// by age (mergedRetention) and by count (maxRetainedMerged, newest kept) — so
// neither a long window nor a busy repo can grow the registry without limit.
// A non-positive retention clears the set entirely, restoring the pre-marker
// behavior. Caller must hold p.mu.
func (p *Poller) sweepMergedLocked(now time.Time) []MergeRequest {
	if p.mergedRetention <= 0 {
		if len(p.merged) > 0 {
			p.merged = make(map[string]MergeRequest)
		}
		return nil
	}

	out := make([]MergeRequest, 0, len(p.merged))
	for key, mr := range p.merged {
		if now.Sub(mergedAnchor(mr)) > p.mergedRetention {
			delete(p.merged, key)
			continue
		}
		out = append(out, mr)
	}

	if len(out) > maxRetainedMerged {
		// Newest merge first; repo/iid breaks ties so which entries the cap
		// evicts does not depend on map iteration order.
		sort.SliceStable(out, func(i, j int) bool {
			ai, aj := mergedAnchor(out[i]), mergedAnchor(out[j])
			if !ai.Equal(aj) {
				return ai.After(aj)
			}
			if out[i].Repo != out[j].Repo {
				return out[i].Repo < out[j].Repo
			}
			return out[i].IID < out[j].IID
		})
		for _, mr := range out[maxRetainedMerged:] {
			delete(p.merged, stampKey(mr.Repo, mr.IID))
		}
		out = out[:maxRetainedMerged]
	}
	return out
}

// mergedAnchor is the timestamp the retention window is measured from: GitLab's
// merged_at when the source reported one, otherwise when this registry first
// classified the MR as merged (last_transition_at, stable across polls).
func mergedAnchor(mr MergeRequest) time.Time {
	if !mr.MergedAt.IsZero() {
		return mr.MergedAt
	}
	return mr.LastTransitionAt
}

// Snapshot returns a copy of the current registry state. Slices and maps are
// copied so callers can't mutate poller internals; MRs/Counts/Projects are
// always non-nil.
func (p *Poller) Snapshot() Snapshot {
	if p == nil {
		return emptySnapshot(nil)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	mrs := make([]MergeRequest, len(p.snap.MergeRequests))
	copy(mrs, p.snap.MergeRequests)

	counts := make(map[string]int, len(p.snap.Counts))
	for k, v := range p.snap.Counts {
		counts[k] = v
	}

	projects := make([]string, len(p.snap.Projects))
	copy(projects, p.snap.Projects)

	return Snapshot{
		MergeRequests: mrs,
		Counts:        counts,
		LastPollAt:    p.snap.LastPollAt,
		Stale:         p.snap.Stale,
		Projects:      projects,
	}
}

// stampKey composes the stable per-MR key used for transition tracking.
func stampKey(repo string, iid int64) string {
	return repo + "!" + strconv.FormatInt(iid, 10)
}
