package mrwatch

import (
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// Env var names. Auth reuses the mills GitLab wiring (GITLAB_API_URL /
// GITLAB_TOKEN) — mrwatch does NOT invent a new token. Only the project set
// and cadence are mrwatch-specific.
const (
	// EnvProjects is a comma-separated list of GitLab project paths to watch.
	// Default: DefaultProject.
	EnvProjects = "LOOM_MRWATCH_PROJECTS"
	// EnvInterval overrides the poll cadence, e.g. "90s", "2m". Default 90s.
	EnvInterval = "LOOM_MRWATCH_INTERVAL"
	// EnvMaxEnrich caps how many MRs per project per poll get the single-MR
	// enrichment GET that carries head_pipeline (the list endpoint returns no
	// pipeline info). Default 20; 0 disables enrichment (list-only view).
	EnvMaxEnrich = "LOOM_MRWATCH_MAX_ENRICH"
	// EnvMergedRetention overrides how long a merged MR is kept in the registry
	// carrying an explicit `merged` state, e.g. "72h", "24h". Default 72h; a
	// value <= 0 disables retention, restoring the behavior where a merged MR
	// vanishes from the registry on the poll after it merges.
	EnvMergedRetention = "LOOM_MRWATCH_MERGED_RETENTION"
	// EnvGitLabAPIURL / EnvGitLabToken are the shared mills GitLab creds.
	EnvGitLabAPIURL = "GITLAB_API_URL"
	EnvGitLabToken  = "GITLAB_TOKEN"
)

// Feature switches for the mrwatch consumers are defined next to their code so
// each slice owns its own env surface, but are catalogued here for discovery:
//
//   - LOOM_MRWATCH_SHEPHERD / LOOM_MRWATCH_SHEPHERD_BUDGET (M4, shepherd.go):
//     the bounded-autonomy reconciler kill switch (default OFF) and per-MR daily
//     action budget (default 2).
//   - LOOM_MRWATCH_NOTIFY (M5, notifier.go): the transition-nudge kill switch
//     (default ON). Gates BOTH the durable agent-context inbox sends AND the
//     read-only attention-lane items; set it to a falsey value
//     ("off"/"0"/"false"/"no"/"disabled") to silence both.
//
// See EnvShepherd/EnvShepherdBudget and EnvNotify for the authoritative names.

// DefaultProject is the repo watched when LOOM_MRWATCH_PROJECTS is unset.
const DefaultProject = "services/loom-core"

// NewPollerFromEnv builds a Poller from the environment, reusing the mills
// GitLab client/token wiring. It returns (nil, nil, nil) when GitLab is not
// configured (no API URL or token) so the HUD boots without MR awareness
// instead of failing — the same degraded-mode contract the mills monitor uses.
// A non-nil error is returned only for a genuinely malformed configuration
// (e.g. a client the clients package rejects), which the caller should log and
// continue past rather than abort HUD init on.
//
// It also builds the M4 shepherd over the SAME GitLab client and registers it
// as the poller's post-poll hook, so a bounded reconcile runs after every poll.
// The shepherd is disabled by default (LOOM_MRWATCH_SHEPHERD must be truthy);
// the returned handle is only needed for the audit endpoint. When GitLab is
// unconfigured the shepherd is nil too.
func NewPollerFromEnv(logger *slog.Logger) (*Poller, *Shepherd, error) {
	if logger == nil {
		logger = slog.Default()
	}
	apiURL := strings.TrimSpace(os.Getenv(EnvGitLabAPIURL))
	token := strings.TrimSpace(os.Getenv(EnvGitLabToken))
	if apiURL == "" || token == "" {
		logger.Info("mrwatch: GitLab not configured (GITLAB_API_URL/GITLAB_TOKEN unset); MR awareness disabled")
		return nil, nil, nil
	}

	projects := ProjectsFromEnv()
	interval := IntervalFromEnv(logger)
	mergedRetention := MergedRetentionFromEnv(logger)

	// The client's Project is the first watched repo; per-project polls use
	// ForProject so the same token scopes to each path.
	base, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:  apiURL,
		Token:   token,
		Project: projects[0],
	})
	if err != nil {
		return nil, nil, err
	}

	src := NewGitLabSource(base)
	src.SetLogger(logger)
	maxEnrich := MaxEnrichFromEnv(logger)
	src.SetMaxEnrich(maxEnrich)
	logger.Info("mrwatch: enabled",
		"projects", strings.Join(projects, ","),
		"interval", interval.String(),
		"max_enrich", maxEnrich,
		"merged_retention", mergedRetention.String())
	poller := NewPoller(src, Options{
		Projects:        projects,
		Interval:        interval,
		MergedRetention: mergedRetention,
		Logger:          logger,
	})

	// Shepherd (M4): shares the same GitLab client for its write actions and
	// reconciles on the poller's cadence via the post-poll hook.
	shepherd := NewShepherdFromEnv(base, logger)
	poller.SetPostPoll(shepherd.Reconcile)

	return poller, shepherd, nil
}

// ProjectsFromEnv parses LOOM_MRWATCH_PROJECTS, falling back to DefaultProject.
// Always returns at least one project.
func ProjectsFromEnv() []string {
	projects := normalizeProjects(splitCSV(os.Getenv(EnvProjects)))
	if len(projects) == 0 {
		return []string{DefaultProject}
	}
	return projects
}

// IntervalFromEnv parses LOOM_MRWATCH_INTERVAL, falling back to DefaultInterval
// on unset or unparseable input.
func IntervalFromEnv(logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvInterval))
	if raw == "" {
		return DefaultInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		if logger != nil {
			logger.Warn("mrwatch: invalid LOOM_MRWATCH_INTERVAL; using default",
				"value", raw, "default", DefaultInterval.String())
		}
		return DefaultInterval
	}
	return d
}

// MergedRetentionFromEnv parses LOOM_MRWATCH_MERGED_RETENTION, falling back to
// DefaultMergedRetention on unset or unparseable input. An explicit value <= 0
// disables retention and is returned as a negative duration, which is how
// Options.MergedRetention spells "off" (its zero value means "use the default").
func MergedRetentionFromEnv(logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvMergedRetention))
	if raw == "" {
		return DefaultMergedRetention
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("mrwatch: invalid LOOM_MRWATCH_MERGED_RETENTION; using default",
				"value", raw, "default", DefaultMergedRetention.String())
		}
		return DefaultMergedRetention
	}
	if d <= 0 {
		return -1
	}
	return d
}

// MaxEnrichFromEnv parses LOOM_MRWATCH_MAX_ENRICH, falling back to the default
// (20) on unset or unparseable input. 0 is valid and disables enrichment;
// negative values clamp to 0.
func MaxEnrichFromEnv(logger *slog.Logger) int {
	raw := strings.TrimSpace(os.Getenv(EnvMaxEnrich))
	if raw == "" {
		return defaultMaxEnrich
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("mrwatch: invalid LOOM_MRWATCH_MAX_ENRICH; using default",
				"value", raw, "default", defaultMaxEnrich)
		}
		return defaultMaxEnrich
	}
	if n < 0 {
		return 0
	}
	return n
}

// normalizeProjects trims, drops empties, dedupes, and sorts for a stable
// watch order.
func normalizeProjects(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}
