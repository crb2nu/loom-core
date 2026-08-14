package codebase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/fsnotify/fsnotify"

	"github.com/crb2nu/loom/pkg/codebase/chunker"
	"github.com/crb2nu/loom/pkg/codebase/index"
	"github.com/crb2nu/loom/pkg/codebase/qdrant"
	"github.com/crb2nu/loom/pkg/codebase/schema"
	"github.com/crb2nu/loom/pkg/validate"
)

type watchJob struct {
	id     string
	cancel context.CancelFunc

	status string
	err    string

	stats schema.WatchStats

	// desc is the durable descriptor for this watch (Root always absolute).
	// Start dedup keys on desc.RepoID + desc.Root.
	desc watchDescriptor
	// lastActive is the last time a client started, reused, or polled this
	// watch. The TTL sweep expires watches idle beyond Config.WatchTTL so an
	// abandoned watch stops re-embedding file changes forever.
	lastActive time.Time
	// lastPersist is when lastActive was last flushed to the watch store.
	lastPersist time.Time
	// claim is the held cross-process run-claim (nil when the store is
	// disabled). Released on any terminal status.
	claim *os.File
}

// watchActivityPersistInterval bounds how often poll activity is flushed to
// the descriptor on disk, so high-frequency polling does not turn into a
// write-per-poll.
const watchActivityPersistInterval = 5 * time.Minute

type watchTask struct {
	absPath string
	relPath string
	op      string // upsert|delete
}

func (s *Service) HandleWatchStart(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	root := validate.StringFromArgs(args, "root", ".")
	repoID := validate.StringFromArgs(args, "repo_id", s.cfg.RepoIDDefault)
	if repoID == "" {
		derived, derr := deriveRepoID(root)
		if derr != nil {
			return nil, derr
		}
		repoID = derived
	}

	// Resolve the root up front: dedup, persistence, and the descriptor all
	// key on the absolute path (the respawned process may have a different
	// cwd). A missing root would otherwise start a zombie watch that persists
	// and resumes forever while watching nothing.
	absRoot := root
	if a, err := filepath.Abs(root); err == nil {
		absRoot = a
	}
	if st, err := os.Stat(absRoot); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("watch root %q is not an existing directory", absRoot)
	}

	langs := s.indexers.SupportedLanguages()
	if normalized := normalizeStringSlice(validate.StringSliceFromArgs(args, "languages")); len(normalized) > 0 {
		langs = normalized
	}
	exclude := validate.StringSliceFromArgs(args, "exclude")

	debounce := 750 * time.Millisecond
	if ms := validate.IntFromArgs(args, "debounce_ms", 0); ms > 0 {
		debounce = time.Duration(ms) * time.Millisecond
	}
	if debounce < 100*time.Millisecond {
		debounce = 100 * time.Millisecond
	}

	gitMetadata := validate.BoolFromArgs(args, "git_metadata", s.cfg.GitMetadataDefault)
	embeddings := validate.BoolFromArgs(args, "embeddings", !s.cfg.DisableEmbeddingsDefault)

	// Cross-process dedup: another server process over the same state dir may
	// already run a watch on this root (the hub spawns one child per client
	// connection). Reuse it instead of stacking another watch that would
	// re-embed every file change in parallel. Orphaned descriptors (owner
	// died) for the same root are cleaned up so the fresh watch replaces them.
	if descriptors, err := s.watchStore.load(); err == nil {
		for _, d := range descriptors {
			if d.RepoID != repoID || d.Root != absRoot || s.hasWatchJob(d.WatchID) {
				continue
			}
			claim, ok := s.watchStore.tryClaim(d.WatchID)
			if !ok {
				return mcp.JSONResult(map[string]any{
					"watch_id":     d.WatchID,
					"repo_id":      repoID,
					"reused":       true,
					"remote":       true,
					"git_metadata": d.GitMetadata,
					"embeddings":   d.Embeddings,
					"hint":         "an equivalent watch is already running in another server process; it was reused instead of starting a duplicate",
				})
			}
			releaseClaim(claim)
			if err := s.watchStore.remove(d.WatchID); err != nil {
				slog.Warn("codebase: remove orphaned watch failed", "watch_id", d.WatchID, "error", err)
			}
		}
	}

	watchID := schema.ShortSHA256Hex(fmt.Sprintf("%s:%d", repoID, time.Now().UnixNano()))

	// Parent the watch goroutine on the service-lifetime context, not the
	// per-request handler context. The handler ctx is the upstream MCP serve
	// context, which is torn down whenever the agent's loom MCP connection
	// drops/reconnects — binding the watcher to it killed long-running
	// watches. baseCtx outlives reconnects yet still cancels on shutdown.
	parent := s.baseCtx
	if parent == nil {
		parent = ctx
	}
	jobCtx, cancel := context.WithCancel(parent)

	startedAt := time.Now()
	desc := watchDescriptor{
		WatchID:      watchID,
		RepoID:       repoID,
		Root:         absRoot,
		Languages:    langs,
		Exclude:      exclude,
		DebounceMs:   int(debounce / time.Millisecond),
		GitMetadata:  gitMetadata,
		Embeddings:   embeddings,
		StartedAt:    startedAt,
		LastActiveAt: startedAt,
	}
	claim, _ := s.watchStore.tryClaim(watchID)
	job := &watchJob{
		id:     watchID,
		cancel: cancel,
		status: "running",
		stats: schema.WatchStats{
			RepoID:    repoID,
			Root:      absRoot,
			StartedAt: startedAt,
		},
		desc:        desc,
		lastActive:  startedAt,
		lastPersist: startedAt,
		claim:       claim,
	}

	// In-process dedup, checked under the same lock as the insert so two
	// concurrent starts on the same root cannot both create a watch. Repeated
	// codebase_watch_start calls (one per agent session is the documented
	// pattern) must not accumulate duplicate watches: each duplicate
	// re-embeds every file change through the paid embedding API.
	s.watchMu.Lock()
	for _, existing := range s.watchJobs {
		if existing.status != "running" || existing.desc.RepoID != repoID || existing.desc.Root != absRoot {
			continue
		}
		existing.lastActive = startedAt
		reusedID := existing.id
		reusedGitMetadata := existing.desc.GitMetadata
		reusedEmbeddings := existing.desc.Embeddings
		s.watchMu.Unlock()
		cancel()
		releaseClaim(claim)
		// No descriptor was saved for the unused id; remove just drops its
		// stray claim file.
		_ = s.watchStore.remove(watchID)
		return mcp.JSONResult(map[string]any{
			"watch_id":     reusedID,
			"repo_id":      repoID,
			"reused":       true,
			"git_metadata": reusedGitMetadata,
			"embeddings":   reusedEmbeddings,
		})
	}
	s.watchJobs[watchID] = job
	s.watchMu.Unlock()

	// Persist the watch so it can be resumed after a process restart.
	if err := s.watchStore.save(desc); err != nil {
		// Non-fatal: the in-memory watch still runs; it just won't survive a
		// restart. Surface it so durability loss is observable.
		slog.Warn("codebase: persist watch failed", "watch_id", watchID, "error", err)
	}

	go s.runWatchJob(jobCtx, watchID, repoID, absRoot, langs, exclude, debounce, gitMetadata, embeddings)

	return mcp.JSONResult(map[string]any{
		"watch_id":     watchID,
		"repo_id":      repoID,
		"git_metadata": gitMetadata,
		"embeddings":   embeddings,
	})
}

// hasWatchJob reports whether watchID is tracked in memory.
func (s *Service) hasWatchJob(watchID string) bool {
	s.watchMu.RLock()
	defer s.watchMu.RUnlock()
	_, ok := s.watchJobs[watchID]
	return ok
}

func (s *Service) HandleWatchPoll(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	watchID := validate.StringFromArgs(args, "watch_id", "")
	if watchID == "" {
		return nil, fmt.Errorf("watch_id is required")
	}

	now := time.Now()
	s.watchMu.Lock()
	job := s.watchJobs[watchID]
	if job != nil {
		// Polling counts as activity: it is the client's signal that the
		// watch is still wanted, which is what keeps it from TTL expiry.
		job.lastActive = now
		needPersist := job.status == "running" && now.Sub(job.lastPersist) >= watchActivityPersistInterval
		var desc watchDescriptor
		if needPersist {
			job.lastPersist = now
			desc = job.desc
			desc.LastActiveAt = now
		}
		resp := map[string]any{
			"found":    true,
			"watch_id": job.id,
			"status":   job.status,
			"error":    job.err,
			"stats":    job.stats,
		}
		s.watchMu.Unlock()
		if needPersist {
			if err := s.watchStore.save(desc); err != nil {
				slog.Warn("codebase: persist watch activity failed", "watch_id", watchID, "error", err)
			}
		}
		return mcp.JSONResult(resp)
	}
	s.watchMu.Unlock()

	// Not tracked in this process. If a persisted descriptor exists, the
	// watch either runs in another server process (claim held) or was
	// orphaned by a dead owner — adopt the orphan here so the watch heals
	// instead of the client starting a duplicate.
	if descriptors, err := s.watchStore.load(); err == nil {
		for _, d := range descriptors {
			if d.WatchID != watchID {
				continue
			}
			claim, ok := s.watchStore.tryClaim(watchID)
			if !ok {
				return mcp.JSONResult(map[string]any{
					"found":    true,
					"watch_id": watchID,
					"status":   "running",
					"remote":   true,
					"hint":     "watch is running in another server process over the same state dir; live stats are only visible from the owning process",
				})
			}
			if st, statErr := os.Stat(d.Root); statErr != nil || !st.IsDir() {
				releaseClaim(claim)
				if rmErr := s.watchStore.remove(watchID); rmErr != nil {
					slog.Warn("codebase: remove watch with missing root failed", "watch_id", watchID, "error", rmErr)
				}
				break
			}
			if s.resumeDescriptor(d, claim) {
				slog.Info("codebase: adopted orphaned watch on poll", "watch_id", watchID, "root", d.Root)
				return mcp.JSONResult(map[string]any{
					"found":    true,
					"watch_id": watchID,
					"status":   "running",
					"resumed":  true,
				})
			}
			break
		}
	}

	return mcp.JSONResult(map[string]any{
		"found":    false,
		"watch_id": watchID,
		"hint":     "watch not found (stopped, expired, or its root no longer exists); codebase_watch_start is idempotent per repo_id+root and will reuse a live watch instead of duplicating",
	})
}

// HandleWatchList reports every watch tracked by this process plus persisted
// watches owned by other processes over the same state dir. Leaked or
// duplicated watches were invisible without this, which is how idle watches
// accumulated and kept re-embedding file changes unnoticed.
func (s *Service) HandleWatchList(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	type watchInfo struct {
		WatchID      string            `json:"watch_id"`
		RepoID       string            `json:"repo_id"`
		Root         string            `json:"root"`
		Status       string            `json:"status"`
		Error        string            `json:"error,omitempty"`
		Embeddings   bool              `json:"embeddings"`
		LastActiveAt time.Time         `json:"last_active_at,omitempty"`
		Stats        schema.WatchStats `json:"stats"`
	}

	s.watchMu.RLock()
	local := make([]watchInfo, 0, len(s.watchJobs))
	running := 0
	for _, job := range s.watchJobs {
		if job.status == "running" {
			running++
		}
		local = append(local, watchInfo{
			WatchID:      job.id,
			RepoID:       job.desc.RepoID,
			Root:         job.desc.Root,
			Status:       job.status,
			Error:        job.err,
			Embeddings:   job.desc.Embeddings,
			LastActiveAt: job.lastActive,
			Stats:        job.stats,
		})
	}
	s.watchMu.RUnlock()
	sort.Slice(local, func(i, j int) bool { return local[i].Stats.StartedAt.Before(local[j].Stats.StartedAt) })

	seen := make(map[string]bool, len(local))
	for _, w := range local {
		seen[w.WatchID] = true
	}
	remote := make([]map[string]any, 0)
	if descriptors, err := s.watchStore.load(); err == nil {
		for _, d := range descriptors {
			if seen[d.WatchID] {
				continue
			}
			remote = append(remote, map[string]any{
				"watch_id":       d.WatchID,
				"repo_id":        d.RepoID,
				"root":           d.Root,
				"started_at":     d.StartedAt,
				"last_active_at": d.LastActiveAt,
			})
		}
	}

	ttl := "disabled"
	if s.cfg.WatchTTL > 0 {
		ttl = s.cfg.WatchTTL.String()
	}
	return mcp.JSONResult(map[string]any{
		"watches":             local,
		"count":               len(local),
		"running":             running,
		"persisted_elsewhere": remote,
		"ttl":                 ttl,
	})
}

func (s *Service) HandleWatchStop(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	watchID := validate.StringFromArgs(args, "watch_id", "")
	if watchID == "" {
		return nil, fmt.Errorf("watch_id is required")
	}

	// Drop the durable descriptor first so a stopped watch never resumes after
	// a restart, even if it is no longer tracked in memory (e.g. resume lost
	// it). Stop is therefore authoritative for cleanup.
	hadDescriptor := false
	if descriptors, err := s.watchStore.load(); err == nil {
		for _, d := range descriptors {
			if d.WatchID == watchID {
				hadDescriptor = true
				break
			}
		}
	}
	if err := s.watchStore.remove(watchID); err != nil {
		slog.Warn("codebase: remove persisted watch failed", "watch_id", watchID, "error", err)
	}

	s.watchMu.RLock()
	job := s.watchJobs[watchID]
	s.watchMu.RUnlock()
	if job == nil {
		if hadDescriptor {
			// The watch may still run in another server process (which holds
			// the flock claim); with the descriptor gone it will never resume
			// again, and the owner's TTL sweep retires it once polls stop.
			return mcp.JSONResult(map[string]any{
				"ok":                true,
				"removed_persisted": true,
				"hint":              "watch was not running in this process; its persisted descriptor was removed so it cannot resume",
			})
		}
		return mcp.JSONResult(map[string]any{"ok": false, "error": "watch job not found"})
	}

	job.cancel()
	return mcp.JSONResult(map[string]any{"ok": true})
}

// ResumeWatches re-launches persisted watches. It is called once at server
// startup so watches survive a process restart (idle-reaper kill,
// transport-storm respawn, crash, or deploy). It also records ctx as the
// service-lifetime parent for new watches, so they outlive per-request handler
// contexts yet stop cleanly on shutdown, and starts the idle-watch janitor.
//
// A descriptor is NOT resumed when: it is idle beyond Config.WatchTTL
// (expired + removed), its root no longer exists (removed — deleted worktrees
// must not resurrect as zombie watches), or another live process holds its
// run-claim (the hub spawns one server child per client connection over a
// shared state dir; resuming everywhere re-embedded every file change once
// per process). Returns the number resumed.
func (s *Service) ResumeWatches(ctx context.Context) int {
	if ctx != nil {
		s.baseCtx = ctx
	}
	if s.baseCtx == nil {
		s.baseCtx = context.Background()
	}
	s.startWatchJanitor()
	if !s.watchStore.enabled() {
		return 0
	}

	descriptors, err := s.watchStore.load()
	if err != nil {
		slog.Warn("codebase: load persisted watches failed", "error", err)
		return 0
	}

	now := time.Now()
	resumed := 0
	for _, d := range descriptors {
		if s.hasWatchJob(d.WatchID) {
			continue
		}
		if s.watchTTLExpired(d, now) {
			// Only reap if no live process owns it: a held claim means the
			// owner is running the watch and manages its own expiry.
			if claim, ok := s.watchStore.tryClaim(d.WatchID); ok {
				releaseClaim(claim)
				if err := s.watchStore.remove(d.WatchID); err != nil {
					slog.Warn("codebase: remove expired watch failed", "watch_id", d.WatchID, "error", err)
				} else {
					slog.Info("codebase: expired idle persisted watch", "watch_id", d.WatchID, "root", d.Root)
				}
			}
			continue
		}
		if st, statErr := os.Stat(d.Root); statErr != nil || !st.IsDir() {
			if claim, ok := s.watchStore.tryClaim(d.WatchID); ok {
				releaseClaim(claim)
				if err := s.watchStore.remove(d.WatchID); err != nil {
					slog.Warn("codebase: remove watch with missing root failed", "watch_id", d.WatchID, "error", err)
				} else {
					slog.Info("codebase: dropped persisted watch with missing root", "watch_id", d.WatchID, "root", d.Root)
				}
			}
			continue
		}
		claim, ok := s.watchStore.tryClaim(d.WatchID)
		if !ok {
			continue
		}
		if s.resumeDescriptor(d, claim) {
			resumed++
		}
	}

	if resumed > 0 {
		slog.Info("codebase: resumed persisted watches", "count", resumed)
	}
	return resumed
}

// resumeDescriptor launches the watch goroutine for a persisted descriptor,
// taking ownership of claim. Returns false (and releases claim) when the
// watch is already tracked in memory.
func (s *Service) resumeDescriptor(d watchDescriptor, claim *os.File) bool {
	langs := d.Languages
	if len(langs) == 0 {
		langs = s.indexers.SupportedLanguages()
	}
	debounce := time.Duration(d.DebounceMs) * time.Millisecond
	if debounce < 100*time.Millisecond {
		debounce = 100 * time.Millisecond
	}

	now := time.Now()
	lastActive := d.LastActiveAt
	if lastActive.IsZero() {
		lastActive = d.StartedAt
	}
	if lastActive.IsZero() {
		lastActive = now
	}

	if s.baseCtx == nil {
		s.baseCtx = context.Background()
	}
	jobCtx, cancel := context.WithCancel(s.baseCtx)
	job := &watchJob{
		id:     d.WatchID,
		cancel: cancel,
		status: "running",
		stats: schema.WatchStats{
			RepoID:    d.RepoID,
			Root:      d.Root,
			StartedAt: d.StartedAt,
		},
		desc:        d,
		lastActive:  lastActive,
		lastPersist: now,
		claim:       claim,
	}

	s.watchMu.Lock()
	if _, exists := s.watchJobs[d.WatchID]; exists {
		s.watchMu.Unlock()
		cancel()
		releaseClaim(claim)
		return false
	}
	s.watchJobs[d.WatchID] = job
	s.watchMu.Unlock()

	go s.runWatchJob(jobCtx, d.WatchID, d.RepoID, d.Root, langs, d.Exclude, debounce, d.GitMetadata, d.Embeddings)
	return true
}

// watchTTLExpired reports whether d has been idle longer than the configured
// watch TTL. A zero TTL disables expiry.
func (s *Service) watchTTLExpired(d watchDescriptor, now time.Time) bool {
	if s.cfg.WatchTTL <= 0 {
		return false
	}
	last := d.LastActiveAt
	if last.IsZero() {
		last = d.StartedAt
	}
	if last.IsZero() {
		return false
	}
	return now.Sub(last) > s.cfg.WatchTTL
}

// startWatchJanitor launches the idle-watch sweep once per process. Without
// it, TTL expiry would only run at process startup, and a long-lived process
// would keep abandoned watches embedding forever.
func (s *Service) startWatchJanitor() {
	if s.cfg.WatchTTL <= 0 {
		return
	}
	s.watchJanitorOnce.Do(func() {
		go s.watchJanitor(s.baseCtx)
	})
}

func (s *Service) watchJanitor(ctx context.Context) {
	interval := s.cfg.WatchTTL / 4
	if interval > time.Hour {
		interval = time.Hour
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.expireIdleWatches(time.Now()); n > 0 {
				slog.Info("codebase: expired idle watches", "count", n)
			}
		}
	}
}

// expireIdleWatches stops running watches whose last client activity (start,
// reuse, or poll) is older than the watch TTL, and removes their persisted
// descriptors so they do not resurrect on the next restart. Returns the
// number expired.
func (s *Service) expireIdleWatches(now time.Time) int {
	if s.cfg.WatchTTL <= 0 {
		return 0
	}

	var victims []*watchJob
	s.watchMu.Lock()
	for _, job := range s.watchJobs {
		if job.status != "running" {
			continue
		}
		last := job.lastActive
		if last.IsZero() {
			last = job.stats.StartedAt
		}
		if last.IsZero() || now.Sub(last) <= s.cfg.WatchTTL {
			continue
		}
		job.status = "expired"
		job.err = fmt.Sprintf("expired after %s without start/poll activity (CODEBASE_WATCH_TTL)", s.cfg.WatchTTL)
		job.stats.StoppedAt = now
		releaseClaim(job.claim)
		job.claim = nil
		victims = append(victims, job)
	}
	s.watchMu.Unlock()

	for _, job := range victims {
		job.cancel()
		if err := s.watchStore.remove(job.id); err != nil {
			slog.Warn("codebase: remove expired watch failed", "watch_id", job.id, "error", err)
		}
	}
	return len(victims)
}

func (s *Service) runWatchJob(
	ctx context.Context,
	watchID string,
	repoID string,
	root string,
	languages []string,
	exclude []string,
	debounce time.Duration,
	gitMetadata bool,
	embeddings bool,
) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		s.setWatchFailed(watchID, fmt.Sprintf("resolve root: %v", err))
		return
	}

	gitRoot := ""
	if gitMetadata {
		if gr, ok := detectGitRoot(ctx, absRoot); ok {
			gitRoot = gr
		} else {
			gitMetadata = false
		}
	}

	vectorSize := 0
	if !embeddings {
		exists, size, err := s.qdrant.GetCollectionVectorSize(ctx)
		if err != nil {
			s.setWatchFailed(watchID, fmt.Sprintf("qdrant collection info: %v", err))
			return
		}
		if exists {
			if size <= 0 {
				s.setWatchFailed(watchID, "qdrant collection vector size unknown")
				return
			}
			vectorSize = size
		} else {
			vectorSize = 1
			if err := s.qdrant.EnsureCollection(ctx, vectorSize); err != nil {
				s.setWatchFailed(watchID, fmt.Sprintf("qdrant ensure collection: %v", err))
				return
			}
		}
	}

	wantExt, err := s.indexers.ExtensionsForLanguages(languages)
	if err != nil {
		s.setWatchFailed(watchID, err.Error())
		return
	}

	ignoreMatcher := index.NewIgnoreMatcher(absRoot, exclude)

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.setWatchFailed(watchID, fmt.Sprintf("fsnotify: %v", err))
		return
	}
	defer fsWatcher.Close()

	addDir := func(dir string) error {
		if err := fsWatcher.Add(dir); err != nil {
			return err
		}
		return nil
	}

	// Watch all directories under root (recursively), skipping excluded paths.
	if err := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return addDir(path)
		}
		if ignoreMatcher.IsIgnored(rel+"/", true) {
			return filepath.SkipDir
		}
		if err := addDir(path); err != nil {
			// Best-effort; keep watching other dirs.
			s.incrementWatchError(watchID, fmt.Sprintf("watch dir %s: %v", rel, err))
			return nil
		}
		return nil
	}); err != nil {
		s.setWatchFailed(watchID, fmt.Sprintf("walk root: %v", err))
		return
	}

	tasks := make(chan watchTask, 2048)
	var wg sync.WaitGroup
	workers := s.cfg.IndexConcurrency
	if workers <= 0 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-tasks:
					if !ok {
						return
					}
					if err := s.applyWatchTask(ctx, watchID, repoID, absRoot, gitRoot, gitMetadata, embeddings, vectorSize, t); err != nil {
						s.incrementWatchError(watchID, err.Error())
					}
				}
			}
		}()
	}

	type pendingInfo struct {
		at time.Time
		op string
	}
	pending := map[string]pendingInfo{}
	var pendingMu sync.Mutex

	enqueue := func(absPath, relPath, op string) {
		pendingMu.Lock()
		pending[absPath] = pendingInfo{at: time.Now(), op: op}
		pendingMu.Unlock()
		s.incrementWatchEvent(watchID)
	}

	flush := func() {
		now := time.Now()
		var ready []watchTask
		pendingMu.Lock()
		for absPath, info := range pending {
			if now.Sub(info.at) < debounce {
				continue
			}
			delete(pending, absPath)
			rel, relErr := filepath.Rel(absRoot, absPath)
			if relErr != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			ready = append(ready, watchTask{absPath: absPath, relPath: rel, op: info.op})
		}
		pendingMu.Unlock()

		if len(ready) == 0 {
			return
		}
		s.incrementWatchQueued(watchID, len(ready))
		for _, t := range ready {
			select {
			case <-ctx.Done():
				return
			case tasks <- t:
			default:
				s.incrementWatchError(watchID, "task queue full; dropping updates")
				return
			}
		}
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			close(tasks)
			wg.Wait()
			s.setWatchStopped(watchID)
			return

		case evt, ok := <-fsWatcher.Events:
			if !ok {
				close(tasks)
				wg.Wait()
				s.setWatchStopped(watchID)
				return
			}
			// Try to watch new directories as they appear.
			if evt.Has(fsnotify.Create) {
				if st, statErr := os.Stat(evt.Name); statErr == nil && st.IsDir() {
					_ = filepath.WalkDir(evt.Name, func(p string, d os.DirEntry, err error) error {
						if err != nil || !d.IsDir() {
							return nil
						}
						rel, relErr := filepath.Rel(absRoot, p)
						if relErr == nil {
							if ignoreMatcher.IsIgnored(filepath.ToSlash(rel)+"/", true) {
								return filepath.SkipDir
							}
						}
						_ = addDir(p)
						return nil
					})
					continue
				}
			}

			rel, relErr := filepath.Rel(absRoot, evt.Name)
			if relErr != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "../") || rel == ".." {
				continue
			}
			if ignoreMatcher.IsIgnored(rel, false) {
				continue
			}

			ext := strings.ToLower(filepath.Ext(evt.Name))
			if ext == "" || !wantExt[ext] {
				continue
			}

			if evt.Has(fsnotify.Remove) || evt.Has(fsnotify.Rename) {
				enqueue(evt.Name, rel, "delete")
				continue
			}
			if evt.Has(fsnotify.Write) || evt.Has(fsnotify.Create) {
				enqueue(evt.Name, rel, "upsert")
				continue
			}

		case err, ok := <-fsWatcher.Errors:
			if ok && err != nil {
				s.incrementWatchError(watchID, fmt.Sprintf("fsnotify error: %v", err))
			}

		case <-ticker.C:
			flush()
		}
	}
}

func (s *Service) applyWatchTask(ctx context.Context, watchID, repoID, absRoot, gitRoot string, gitMetadata bool, embeddings bool, vectorSize int, t watchTask) error {
	stages := schema.WatchStageStats{}
	switch t.op {
	case "delete":
		deleteStart := time.Now()
		if err := s.qdrant.DeleteFile(ctx, repoID, t.relPath); err != nil && !errors.Is(err, qdrant.ErrCollectionNotFound) {
			return fmt.Errorf("delete %s: %v", t.relPath, err)
		}
		stages.DeleteBeforeUpsert = stageSample(time.Since(deleteStart), 1)
		s.mergeWatchStageStats(watchID, stages)
		s.incrementWatchDeleted(watchID)
		return nil
	default:
		if _, err := os.Stat(t.absPath); err != nil {
			deleteStart := time.Now()
			if err := s.qdrant.DeleteFile(ctx, repoID, t.relPath); err != nil && !errors.Is(err, qdrant.ErrCollectionNotFound) {
				return fmt.Errorf("delete missing %s: %v", t.relPath, err)
			}
			stages.DeleteBeforeUpsert = stageSample(time.Since(deleteStart), 1)
			s.mergeWatchStageStats(watchID, stages)
			s.incrementWatchDeleted(watchID)
			return nil
		}

		readStart := time.Now()
		b, err := os.ReadFile(t.absPath)
		if err != nil {
			return fmt.Errorf("read %s: %v", t.relPath, err)
		}
		stages.FileRead = stageSample(time.Since(readStart), 1)
		if s.cfg.MaxFileBytes > 0 && int64(len(b)) > s.cfg.MaxFileBytes {
			s.mergeWatchStageStats(watchID, stages)
			return nil
		}

		fileHash := schema.ContentHashBytes(b)
		preflightStart := time.Now()
		preflight, preflightErr := s.qdrant.GetFilePreflight(ctx, repoID, t.relPath, s.cfg.EmbedModel, 4096)
		stages.PreflightLookup = stageSample(time.Since(preflightStart), 1)
		stages.UnchangedHashLookup = stageSample(0, 1)
		if embeddings {
			stages.EmbeddingCacheLookup = stageSample(0, 1)
		}
		if preflightErr == nil && preflight.ModuleFound && preflight.ModuleContentHash == fileHash {
			s.mergeWatchStageStats(watchID, stages)
			s.incrementWatchSkipped(watchID)
			return nil
		}

		var fileCache map[string][]float64
		if preflightErr != nil {
			s.incrementWatchError(watchID, fmt.Sprintf("preflight %s: %v", t.relPath, preflightErr))
		} else if embeddings {
			fileCache = preflight.EmbeddingCache
		}

		deleteStart := time.Now()
		if delErr := s.qdrant.DeleteFile(ctx, repoID, t.relPath); delErr != nil && !errors.Is(delErr, qdrant.ErrCollectionNotFound) {
			return fmt.Errorf("delete before upsert %s: %v", t.relPath, delErr)
		}
		stages.DeleteBeforeUpsert = stageSample(time.Since(deleteStart), 1)

		parseStart := time.Now()
		chunks, err := s.indexers.IndexFileFromContent(ctx, absRoot, t.absPath, repoID, b)
		if err != nil {
			return fmt.Errorf("index %s: %v", t.relPath, err)
		}
		stages.ParseIndex = stageSample(time.Since(parseStart), 1)
		if len(chunks) == 0 {
			s.mergeWatchStageStats(watchID, stages)
			return nil
		}

		if gitMetadata {
			gitStart := time.Now()
			if err := annotateChunksWithGitMetadata(ctx, gitRoot, t.absPath, chunks); err != nil {
				s.incrementWatchError(watchID, fmt.Sprintf("git metadata %s: %v", t.relPath, err))
			}
			stages.GitMetadata = stageSample(time.Since(gitStart), len(chunks))
		}

		chunkStart := time.Now()
		chunks = chunker.SplitLargeChunks(chunks, chunker.Config{
			MaxTokens:     s.cfg.ChunkMaxTokens,
			OverlapTokens: s.cfg.ChunkOverlapTokens,
			MinTokens:     s.cfg.ChunkMinTokens,
		})
		for i := range chunks {
			chunker.EnrichChunkIdentifiers(&chunks[i])
		}
		stages.ChunkSplitEnrich = stageSample(time.Since(chunkStart), len(chunks))

		points := make([]qdrant.Point, 0, len(chunks))
		embedModel := ""
		if embeddings {
			embedModel = s.cfg.EmbedModel
		}
		if embeddings {
			vectors := make([][]float64, len(chunks))
			var (
				texts   []string
				indices []int
			)
			for i, ch := range chunks {
				if fileCache != nil {
					if v, ok := fileCache[ch.ContentHash]; ok && len(v) > 0 {
						vectors[i] = v
						continue
					}
				}
				text := ch.Content
				if ch.Docstring != "" {
					text = ch.Docstring + "\n\n" + text
				}
				texts = append(texts, text)
				indices = append(indices, i)
			}

			if len(texts) > 0 {
				embedStart := time.Now()
				embedded, err := s.embed.EmbedDocuments(ctx, texts)
				if err != nil {
					return fmt.Errorf("embed %s: %v", t.relPath, err)
				}
				stages.Embedding = stageSample(time.Since(embedStart), len(texts))
				if len(embedded) != len(texts) {
					return fmt.Errorf("embed %s: returned %d vectors for %d texts", t.relPath, len(embedded), len(texts))
				}
				for j, idx := range indices {
					vectors[idx] = embedded[j]
				}
			}

			size := 0
			for _, v := range vectors {
				if len(v) > 0 {
					size = len(v)
					break
				}
			}
			if size <= 0 {
				return fmt.Errorf("embed %s: empty vector", t.relPath)
			}
			if err := s.ensureCollectionForVector(ctx, size, false); err != nil {
				return fmt.Errorf("ensure collection: %v", err)
			}

			for i := range chunks {
				if err := checkVectorDim(chunks[i].ID, vectors[i], size); err != nil {
					return fmt.Errorf("embed %s: %w", t.relPath, err)
				}
				points = append(points, qdrant.Point{
					ID:      chunks[i].ID,
					Vector:  vectors[i],
					Payload: qdrant.ChunkToPayload(chunks[i], true, embedModel),
				})
			}
		} else {
			if vectorSize <= 0 {
				return fmt.Errorf("no-embeddings mode requires known qdrant vector size")
			}
			dummy := make([]float64, vectorSize)
			dummy[0] = 1
			for i := range chunks {
				payload := qdrant.ChunkToPayload(chunks[i], true, embedModel)
				payload[qdrant.EmbeddingFallbackPayloadKey] = true
				points = append(points, qdrant.Point{
					ID:      chunks[i].ID,
					Vector:  dummy,
					Payload: payload,
				})
			}
		}

		upsertStart := time.Now()
		for i := 0; i < len(points); i += s.cfg.UpsertBatchSize {
			end := i + s.cfg.UpsertBatchSize
			if end > len(points) {
				end = len(points)
			}
			// Mirror index_pipeline.go: all bulk batches wait=false.
			// Watch events are typically a single small batch so
			// throughput gain is modest, but the symmetry keeps both
			// code paths predictable and avoids per-call EOF traps.
			// Operators can force wait=true via CODEBASE_UPSERT_BLOCKING
			// (rollback hatch; CODEBASE_UPSERT_WAIT still honored).
			if err := s.qdrant.Upsert(ctx, points[i:end], s.cfg.UpsertBlocking); err != nil {
				return fmt.Errorf("upsert %s: %v", t.relPath, err)
			}
		}
		stages.QdrantUpsert = stageSample(time.Since(upsertStart), len(points))

		// Per-watch-event durability flush. Treated as a soft warning if
		// it fails — prior writes are durable via WAL fsync
		// (flush_interval_sec=5 default). We do NOT propagate the error
		// because doing so would mark the watch event failed despite the
		// data having landed in Qdrant.
		if flushErr := s.qdrant.Flush(ctx); flushErr != nil {
			s.incrementWatchError(watchID, fmt.Sprintf("watch flush %s: %v", t.relPath, flushErr))
		}

		s.mergeWatchStageStats(watchID, stages)
		s.incrementWatchIndexed(watchID, len(chunks))
		return nil
	}
}
