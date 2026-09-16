package health

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
)

const RemediationLabel = "remediation"

// Advisory identifies the one dependency update required by govulncheck.
type Advisory struct {
	ID, Module, FixedVersion string
}

func (a Advisory) Identity() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{a.ID, a.Module, a.FixedVersion}, "\x00")))
	return "remediation-" + hex.EncodeToString(sum[:12])
}

// ParseGovulncheck parses govulncheck's streaming JSON protocol. Only findings
// with a module and an advisory range containing a fixed version are actionable.
func ParseGovulncheck(r io.Reader) ([]Advisory, error) {
	type event struct {
		OSV *struct {
			ID       string `json:"id"`
			Affected []struct {
				Module struct {
					Path string `json:"path"`
				} `json:"module"`
				Ranges []struct {
					Events []struct {
						Fixed string `json:"fixed"`
					} `json:"events"`
				} `json:"ranges"`
			} `json:"affected"`
		} `json:"osv"`
		Finding *struct {
			OSV   string `json:"osv"`
			Trace []struct {
				Module  string `json:"module"`
				Version string `json:"version"`
			} `json:"trace"`
		} `json:"finding"`
	}
	osvs := map[string]Advisory{}
	found := map[string]bool{}
	dec := json.NewDecoder(r)
	for {
		var e event
		if err := dec.Decode(&e); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode govulncheck report: %w", err)
		}
		if e.OSV != nil {
			for _, affected := range e.OSV.Affected {
				for _, rng := range affected.Ranges {
					for _, ev := range rng.Events {
						if e.OSV.ID != "" && affected.Module.Path != "" && ev.Fixed != "" {
							osvs[e.OSV.ID+"\x00"+affected.Module.Path] = Advisory{ID: e.OSV.ID, Module: affected.Module.Path, FixedVersion: ev.Fixed}
						}
					}
				}
			}
		}
		if e.Finding != nil {
			for _, tr := range e.Finding.Trace {
				if tr.Module != "" {
					found[e.Finding.OSV+"\x00"+tr.Module] = true
				}
			}
		}
	}
	var out []Advisory
	for key, advisory := range osvs {
		if found[key] {
			out = append(out, advisory)
		}
	}
	// Map iteration is random; the remediator acts on out[0], so a stable order
	// keeps repeated ticks converging on the same advisory.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Module < out[j].Module
	})
	return out, nil
}

type RemediationIncident struct {
	PipelineID int64
	JobName    string
	Report     []byte
}

type RemediationMR struct {
	IID                          int64
	HeadSHA, Title, SourceBranch string
	AutoMerge                    bool
}

type RemediationSource interface {
	LatestAdvisoryRed(context.Context) (*RemediationIncident, error)
	OpenRenovateMRs(context.Context) ([]RemediationMR, error)
}

type RemediationSink interface {
	ArtifactExists(context.Context, string) (bool, error)
	PrioritizeAndArm(context.Context, RemediationMR, Advisory) error
	CreateBacklog(context.Context, string, Advisory, []string) error
}

type RemediationResult struct {
	Created, Armed bool
	Identity       string
}

// Remediator performs at most one write per tick. RenovateEnabled is a
// deployment capability decision, not a runtime guess.
type Remediator struct {
	Source          RemediationSource
	Sink            RemediationSink
	RenovateEnabled bool
	Interval        time.Duration
	// Log receives tick failures; a nil logger drops them, which is the
	// silent mode the S5 review found unacceptable for an unattended loop.
	Log *slog.Logger
}

func (r *Remediator) Tick(ctx context.Context) (RemediationResult, error) {
	incident, err := r.Source.LatestAdvisoryRed(ctx)
	if err != nil || incident == nil {
		return RemediationResult{}, err
	}
	if incident.JobName != "security:govulncheck" {
		return RemediationResult{}, nil
	}
	advisories, err := ParseGovulncheck(bytes.NewReader(incident.Report))
	if err != nil || len(advisories) == 0 {
		return RemediationResult{}, err
	}
	a := advisories[0] // one bounded artifact per tick
	id := a.Identity()
	exists, err := r.Sink.ArtifactExists(ctx, id)
	if err != nil || exists {
		return RemediationResult{Identity: id}, err
	}
	if r.RenovateEnabled {
		mrs, listErr := r.Source.OpenRenovateMRs(ctx)
		if listErr != nil {
			return RemediationResult{}, listErr
		}
		for _, mr := range mrs {
			haystack := strings.ToLower(mr.Title + " " + mr.SourceBranch)
			if strings.Contains(haystack, strings.ToLower(a.Module)) && strings.Contains(haystack, strings.ToLower(a.FixedVersion)) {
				if mr.AutoMerge {
					return RemediationResult{Identity: id}, nil
				}
				if strings.TrimSpace(mr.HeadSHA) == "" {
					return RemediationResult{}, errors.New("matching Renovate MR has no observed head SHA")
				}
				if err := r.Sink.PrioritizeAndArm(ctx, mr, a); err != nil {
					return RemediationResult{}, err
				}
				return RemediationResult{Armed: true, Identity: id}, nil
			}
		}
		return RemediationResult{Identity: id}, nil
	}
	if err := r.Sink.CreateBacklog(ctx, id, a, []string{RemediationLabel}); err != nil {
		return RemediationResult{}, err
	}
	return RemediationResult{Created: true, Identity: id}, nil
}

func (r *Remediator) tickLogged(ctx context.Context) {
	res, err := r.Tick(ctx)
	if r.Log == nil {
		return
	}
	switch {
	case err != nil && ctx.Err() == nil:
		r.Log.Warn("health remediator tick failed", "error", err)
	case res.Created || res.Armed:
		r.Log.Info("health remediator acted", "identity", res.Identity, "created", res.Created, "armed", res.Armed)
	}
}

func (r *Remediator) Run(ctx context.Context) error {
	r.tickLogged(ctx)
	interval := r.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tickLogged(ctx)
		}
	}
}
