// Command gatekeeper-probe is the kill-test harness for the "Merge Gatekeeper"
// product thesis (.loom/local/strategy-mills-next-level-2026-07-25.md): run the
// Mills gate suite against merge requests that were NOT authored by Mills — no
// slice scope, no backlog metadata — and report which gates still produce
// signal. It is read-only against GitLab and calls the same litellm judge the
// operator uses for the two LLM gates.
//
// Usage:
//
//	GITLAB_TOKEN=... LITELLM_KEY=... go run ./tools/gatekeeper-probe \
//	  -project services/flexdeck -count 5
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type mrSummary struct {
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	Author       struct{ Username string }
	UserNotes    int    `json:"user_notes_count"`
	SHA          string `json:"sha"`
	MergedAt     string `json:"merged_at"`
	HeadPipeline *struct {
		Status string `json:"status"`
	} `json:"head_pipeline"`
}

type mrChange struct {
	NewPath string `json:"new_path"`
	Diff    string `json:"diff"`
}

type mrCommit struct {
	Message string `json:"message"`
}

func gitlabGET(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://gitlab.flexinfer.ai/api/v4"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d: %.200s", path, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

func main() {
	project := flag.String("project", "services/flexdeck", "GitLab project path")
	count := flag.Int("count", 5, "number of recent merged MRs to probe")
	flag.Parse()

	token := os.Getenv("GITLAB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "GITLAB_TOKEN required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	proj := url.PathEscape(*project)
	var mrs []mrSummary
	if err := gitlabGET(ctx, token,
		"/projects/"+proj+"/merge_requests?state=merged&order_by=updated_at&per_page="+fmt.Sprint(*count), &mrs); err != nil {
		fmt.Fprintln(os.Stderr, "list MRs:", err)
		os.Exit(1)
	}

	reg := gates.Default()
	var judgeReady bool
	// PROBE_JUDGE=anthropic swaps the rubric judge to the operator's
	// tiebreaker model so the discrimination mutants can attribute weak
	// verdicts to the judge model rather than the gate machinery.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" && os.Getenv("PROBE_JUDGE") == "anthropic" {
		ac, err := clients.NewAnthropicClient(clients.AnthropicClientConfig{
			APIKey:  key,
			Timeout: 2 * time.Minute,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "anthropic client:", err)
			os.Exit(1)
		}
		gates.RegisterLLMGates(reg, &clients.AnthropicRubricJudge{Client: ac, Model: "claude-sonnet-5"})
		judgeReady = true
	} else if key := os.Getenv("LITELLM_KEY"); key != "" {
		// PROBE_JUDGE_URL / PROBE_JUDGE_MODEL generalize the OpenAI-compatible
		// path so the bake-off can point at any /v1/chat/completions backend
		// (litellm, api.openai.com) with any model, no fallback chain.
		proxyURL := os.Getenv("PROBE_JUDGE_URL")
		if proxyURL == "" {
			proxyURL = "https://litellm.flexinfer.ai"
		}
		model := os.Getenv("PROBE_JUDGE_MODEL")
		var fallbacks []string
		if model == "" {
			model = "or/qwen-2.5-72b"
			fallbacks = []string{"or/kimi-k2.7-code"}
		}
		fc, err := clients.NewFlexInferClient(clients.FlexInferConfig{
			ProxyURL:            proxyURL,
			Token:               key,
			JudgeModel:          model,
			JudgeModelFallbacks: fallbacks,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "judge client:", err)
		} else {
			gates.RegisterLLMGates(reg, clients.NewRubricJudge(fc))
			judgeReady = true
		}
	}

	gateNames := []string{"nonempty_diff", "diff_size", "scope", "path_policy", "secret_scan", "commit_format", "docs_guardrail"}
	if judgeReady {
		gateNames = append(gateNames, "spec_conformance", "pr_self_review")
	}

	type verdict struct {
		Gate    string `json:"gate"`
		Pass    bool   `json:"pass"`
		Skip    bool   `json:"skip,omitempty"`
		Detail  string `json:"detail,omitempty"`
		Err     string `json:"err,omitempty"`
		Elapsed string `json:"elapsed"`
	}
	type mrReport struct {
		IID      int       `json:"iid"`
		Title    string    `json:"title"`
		Author   string    `json:"author"`
		Notes    int       `json:"user_notes_count"`
		Pipeline string    `json:"head_pipeline"`
		Files    int       `json:"files_changed"`
		Added    int       `json:"lines_added"`
		Removed  int       `json:"lines_removed"`
		Verdicts []verdict `json:"verdicts"`
	}
	var reports []mrReport
	var inputs []gates.StageInput

	for _, mr := range mrs {
		var changes struct {
			Changes []mrChange `json:"changes"`
		}
		if err := gitlabGET(ctx, token,
			fmt.Sprintf("/projects/%s/merge_requests/%d/changes", proj, mr.IID), &changes); err != nil {
			fmt.Fprintln(os.Stderr, "changes:", err)
			continue
		}
		var commits []mrCommit
		if err := gitlabGET(ctx, token,
			fmt.Sprintf("/projects/%s/merge_requests/%d/commits", proj, mr.IID), &commits); err != nil {
			fmt.Fprintln(os.Stderr, "commits:", err)
			continue
		}

		var files []string
		var patch strings.Builder
		added, removed := 0, 0
		for _, c := range changes.Changes {
			files = append(files, c.NewPath)
			patch.WriteString("--- a/" + c.NewPath + "\n+++ b/" + c.NewPath + "\n")
			patch.WriteString(c.Diff)
			for _, line := range strings.Split(c.Diff, "\n") {
				switch {
				case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
					added++
				case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
					removed++
				}
			}
		}
		var msgs []string
		for _, c := range commits {
			msgs = append(msgs, c.Message)
		}
		pipelineStatus := ""
		if mr.HeadPipeline != nil {
			pipelineStatus = mr.HeadPipeline.Status
		}

		in := gates.StageInput{
			Item: &store.BacklogItem{
				ID:      fmt.Sprintf("probe-%s-%d", strings.ReplaceAll(*project, "/", "-"), mr.IID),
				Title:   mr.Title,
				SpecDoc: mr.Description,
			},
			Policy:         &mills.Policy{},
			FilesChanged:   files,
			LinesAdded:     added,
			LinesRemoved:   removed,
			DiffPatch:      []byte(patch.String()),
			CommitMessages: msgs,
			TestsPassed:    pipelineStatus == "success",
		}

		rep := mrReport{
			IID: mr.IID, Title: mr.Title, Author: mr.Author.Username,
			Notes: mr.UserNotes, Pipeline: pipelineStatus,
			Files: len(files), Added: added, Removed: removed,
		}
		for _, name := range gateNames {
			g, gerr := reg.Get(name)
			if gerr != nil {
				continue
			}
			start := time.Now()
			out, err := g.Evaluate(ctx, in)
			v := verdict{Gate: name, Pass: out.Pass, Skip: out.Skip, Detail: strings.Join(out.Reasons, "; "), Elapsed: time.Since(start).Round(time.Millisecond).String()}
			if err != nil {
				v.Err = err.Error()
			}
			rep.Verdicts = append(rep.Verdicts, v)
		}
		reports = append(reports, rep)
		inputs = append(inputs, in)
	}

	// Mutation probes: the discrimination half of the kill-test. Passing
	// clean MRs only proves the absence of false positives; these feed the
	// gates deliberately-bad inputs derived from the real MRs and record
	// whether each responsible gate actually fails.
	if len(inputs) >= 2 {
		runMutant := func(label string, in gates.StageInput, gateName string) {
			g, gerr := reg.Get(gateName)
			if gerr != nil {
				return
			}
			start := time.Now()
			out, err := g.Evaluate(ctx, in)
			v := verdict{Gate: gateName, Pass: out.Pass, Skip: out.Skip,
				Detail:  strings.Join(out.Reasons, "; "),
				Elapsed: time.Since(start).Round(time.Millisecond).String()}
			if err != nil {
				v.Err = err.Error()
			}
			reports = append(reports, mrReport{Title: "MUTANT: " + label, Verdicts: []verdict{v}})
		}

		// Cross-wired spec: MR[0]'s diff graded against MR[1]'s description.
		crossed := inputs[0]
		crossedItem := *crossed.Item
		crossedItem.Title = inputs[1].Item.Title
		crossedItem.SpecDoc = inputs[1].Item.SpecDoc
		crossed.Item = &crossedItem
		if judgeReady {
			runMutant("cross-wired spec (diff[0] vs spec[1])", crossed, "spec_conformance")
		}

		// Absurd spec: same diff graded against a spec it cannot possibly
		// implement. If the judge passes this, the gate has no
		// discrimination without Mills' richer stage context.
		absurd := inputs[0]
		absurdItem := *absurd.Item
		absurdItem.Title = "Implement a Redis-backed rate limiter for the ingest API"
		absurdItem.SpecDoc = "Add a Redis-backed sliding-window rate limiter to the ingest API: " +
			"new middleware in internal/ratelimit, config knobs RATE_LIMIT_RPS and RATE_LIMIT_BURST, " +
			"429 responses with Retry-After, and unit tests covering window rollover and burst exhaustion."
		absurd.Item = &absurdItem
		if judgeReady {
			runMutant("absurd spec (redis rate limiter vs a11y diff)", absurd, "spec_conformance")
		}

		// Planted credential in an added line.
		leaked := inputs[0]
		// The planted Stripe key is assembled from fragments so repo-hosting
		// secret scanners do not flag the probe's own source as a leak.
		leaked.DiffPatch = append(append([]byte{}, leaked.DiffPatch...),
			[]byte("\n+aws_secret_access_key = \"AKIAIOSFODNN7EXAMPLEKEY99\"\n+export STRIPE_KEY="+
				"sk_live_"+"51Hq9zXFakeButRealShaped000\n")...)
		runMutant("planted credentials in diff", leaked, "secret_scan")

		// Non-conventional commit message.
		badCommit := inputs[0]
		badCommit.CommitMessages = []string{"wip stuff, will fix later"}
		runMutant("non-conventional commit message", badCommit, "commit_format")

		// Empty diff claiming the same spec.
		empty := inputs[0]
		empty.DiffPatch = nil
		empty.FilesChanged = nil
		empty.LinesAdded, empty.LinesRemoved = 0, 0
		runMutant("empty diff", empty, "nonempty_diff")
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(reports)
}
