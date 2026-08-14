package clients_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// This is the slice-2 completion criterion for #374, wired end to end: a real
// pipeline.Runner, a real clients.GitLabClient, and a fake GitLab over
// httptest. The MR head moves AFTER ci_watch goes green, and the run must
//
//	1. record exactly one external head transition,
//	2. rewind exactly once to the first source-sensitive stage,
//	3. re-reach a fresh green ci_watch for the successor SHA,
//	4. merge exactly once, with the wire-level `sha` precondition on the merge
//	   PUT equal to the SUCCESSOR — never the SHA the first CI run tested.
//
// Asserting the literal merge body is the point: the whole feature exists so a
// merge can only ever be bound to a revision that was actually tested.

const (
	e2eProject      = "services/loom-core"
	e2eProjectPath  = "services%2Floom-core"
	e2eMRIID        = 77
	e2eReviewedSHA  = "1111111111111111111111111111111111111111"
	e2eSuccessorSHA = "2222222222222222222222222222222222222222"
	e2eMergeCommit  = "3333333333333333333333333333333333333333"
)

// fakeGitLab is an httptest-backed GitLab whose MR head can move under the
// caller's feet — exactly the race #374 exists to survive.
type fakeGitLab struct {
	mu sync.Mutex
	t  *testing.T

	head   string
	merged bool

	// moveHeadAfterCIPolls flips the head to e2eSuccessorSHA once this many
	// terminal pipeline lookups have been served, simulating an external push
	// landing in the window between "CI went green" and "merge".
	moveHeadAfterCIPolls int

	terminalPipelineReads int
	mergePUTBodies        []string
	mrCreates             int
	branchDeletes         int
}

func (f *fakeGitLab) currentHead() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head
}

func (f *fakeGitLab) mergeSHAs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.mergePUTBodies))
	for _, body := range f.mergePUTBodies {
		var parsed struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			f.t.Fatalf("merge body is not json: %v", err)
		}
		out = append(out, parsed.SHA)
	}
	return out
}

func (f *fakeGitLab) mrBody() map[string]any {
	state := "opened"
	body := map[string]any{
		"iid":                e2eMRIID,
		"web_url":            "https://gitlab.example/" + e2eProject + "/-/merge_requests/77",
		"sha":                f.head,
		"source_branch":      "feat/BL-E2E-374",
		"target_branch":      "main",
		"rebase_in_progress": false,
		"merge_error":        nil,
	}
	if f.merged {
		state = "merged"
		body["merge_commit_sha"] = e2eMergeCommit
	}
	body["state"] = state
	return body
}

func (f *fakeGitLab) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath keeps the %2F-encoded project segment intact; the decoded
		// Path would turn "services%2Floom-core" into two path elements.
		path := r.URL.EscapedPath()
		base := "/api/v4/projects/" + e2eProjectPath
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && path == base+"/merge_requests":
			writeE2EJSON(w, 200, []any{})

		case r.Method == http.MethodPost && path == base+"/merge_requests":
			f.mrCreates++
			writeE2EJSON(w, 201, f.mrBody())

		case r.Method == http.MethodGet && path == base+"/merge_requests/77":
			writeE2EJSON(w, 200, f.mrBody())

		case r.Method == http.MethodGet && path == base+"/pipelines":
			q := r.URL.Query()
			sha := q.Get("sha")
			if sha != f.head {
				// No pipeline exists for a SHA that is not the head.
				writeE2EJSON(w, 200, []any{})
				return
			}
			f.terminalPipelineReads++
			writeE2EJSON(w, 200, []any{map[string]any{
				"id":      1000 + f.terminalPipelineReads,
				"sha":     sha,
				"ref":     q.Get("ref"),
				"status":  "success",
				"source":  q.Get("source"),
				"web_url": "https://gitlab.example/pipelines/1",
			}})
			if f.terminalPipelineReads == f.moveHeadAfterCIPolls {
				// The external push lands here: ci_watch has just been handed a
				// green verdict for e2eReviewedSHA, and by the time merge reads
				// the MR the head is somewhere else entirely.
				f.head = e2eSuccessorSHA
			}

		case r.Method == http.MethodGet && path == base+"/repository/branches/feat%2FBL-E2E-374":
			writeE2EJSON(w, 200, map[string]any{
				"name":   "feat/BL-E2E-374",
				"commit": map[string]any{"id": f.head},
			})

		case r.Method == http.MethodPut && path == base+"/merge_requests/77/merge":
			body := readE2EBody(r)
			f.mergePUTBodies = append(f.mergePUTBodies, body)
			var parsed struct {
				SHA string `json:"sha"`
			}
			_ = json.Unmarshal([]byte(body), &parsed)
			if parsed.SHA != f.head {
				// GitLab's own SHA precondition. Reaching this at all would mean
				// Mills tried to merge a revision the branch no longer holds.
				writeE2EJSON(w, 409, map[string]any{"message": "SHA does not match HEAD of source branch"})
				return
			}
			f.merged = true
			writeE2EJSON(w, 200, f.mrBody())

		case r.Method == http.MethodDelete && strings.HasPrefix(path, base+"/repository/branches/"):
			f.branchDeletes++
			w.WriteHeader(204)

		default:
			f.t.Errorf("unexpected %s %s", r.Method, path)
			writeE2EJSON(w, 404, map[string]any{"message": "404 Not Found"})
		}
	})
}

func writeE2EJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func readE2EBody(r *http.Request) string {
	if r.Body == nil {
		return "{}"
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// implementWorker stands in for the spawn/devbox stages so the DAG can run
// without a cluster. It supplies the diff evidence the source-sensitive gates
// consume after a rewind.
type implementWorker struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *implementWorker) Run(_ context.Context, jc pipeline.JobContext) (pipeline.StageOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[jc.Stage.ID]++
	if jc.Stage.ID == "implement" {
		return pipeline.StageOutput{
			FilesChanged:   []string{"pkg/mills/store/dao_mr_transitions.go"},
			DiffPatch:      []byte("diff --git a/x b/x\n+durable\n"),
			CommitMessages: []string{"feat(mills): durable head transitions"},
		}, nil
	}
	return pipeline.StageOutput{}, nil
}

func (s *implementWorker) count(stage string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[stage]
}

func TestE2E_HeadMovesAfterGreenCI_RegatesAndMergesTheSuccessor(t *testing.T) {
	ctx := context.Background()
	fake := &fakeGitLab{t: t, head: e2eReviewedSHA, moveHeadAfterCIPolls: 1}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	cli, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:       srv.URL + "/api/v4",
		Token:        "tok-e2e",
		Project:      e2eProject,
		PollInterval: 5 * time.Millisecond,
		PollDeadline: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("gitlab client: %v", err)
	}

	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	item := &store.BacklogItem{
		ID:       "BL-E2E-374",
		Title:    "durable head transitions",
		State:    store.BacklogRunning,
		Priority: store.P2,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{
		ID:        "PIPE-BL-E2E-374-0",
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     store.PipelineQueued,
		Attempts:  1,
		StartedAt: time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stub := &implementWorker{}
	gw := &pipeline.GitLabWorker{Client: cli}
	disp := pipeline.NewDispatcher(map[string]pipeline.Worker{
		"mr":       gw,
		"ci_watch": gw,
		"merge":    gw,
		"cleanup":  gw,
	}, stub)
	r := pipeline.New(st, gates.NewRegistry(), disp, nil)

	// ----- drive 1: green CI, then the head moves, then merge refuses -----
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("first drive: %v", err)
	}

	rows, err := st.MRHeadTransitions.ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("head transitions after drive 1 = %d, want exactly 1", len(rows))
	}
	if rows[0].ReviewedSHA != e2eReviewedSHA || rows[0].SuccessorSHA != e2eSuccessorSHA {
		t.Fatalf("transition must name both SHAs: %+v", rows[0])
	}
	if rows[0].Trigger != store.MRHeadTriggerExternal {
		t.Errorf("trigger = %q, want external", rows[0].Trigger)
	}

	rewound, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if rewound.CurrentStage != "post_implement_gate" {
		t.Fatalf("current_stage after the movement = %q, want post_implement_gate", rewound.CurrentStage)
	}
	if rewound.State == store.PipelineDone || rewound.State == store.PipelineEscalated {
		t.Fatalf("run terminalized at %q; the first movement must re-gate", rewound.State)
	}
	if got := fake.mergeSHAs(); len(got) != 0 {
		t.Fatalf("merge PUTs issued before the re-gate: %v — the identity check must refuse before any mutation", got)
	}
	if got := fake.currentHead(); got != e2eSuccessorSHA {
		t.Fatalf("fake head = %q, want the successor", got)
	}

	// ----- drive 2: re-gate, fresh CI on the successor, then merge -----
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("second drive: %v", err)
	}

	done, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if done.State != store.PipelineDone {
		t.Fatalf("final state = %q, want done", done.State)
	}

	// Exactly one merge, bound to the SHA the SECOND ci_watch tested.
	mergeSHAs := fake.mergeSHAs()
	if len(mergeSHAs) != 1 {
		t.Fatalf("merge PUTs = %d (%v), want exactly 1", len(mergeSHAs), mergeSHAs)
	}
	if mergeSHAs[0] != e2eSuccessorSHA {
		t.Errorf("merge body sha = %q, want the successor %q — the merge must bind to the revision that was actually tested", mergeSHAs[0], e2eSuccessorSHA)
	}

	// Exactly one rewind: one extra pass over the source-sensitive tail, and
	// no re-run of the expensive planning/implementation stages.
	if got := stub.count("implement"); got != 1 {
		t.Errorf("implement ran %d times, want 1 — a rewind replays the same work onto a new base", got)
	}
	if got := stub.count("plan_slice"); got != 1 {
		t.Errorf("plan_slice ran %d times, want 1", got)
	}
	if got := stub.count("tests"); got != 2 {
		t.Errorf("tests ran %d times, want 2 (once per gate pass over the branch)", got)
	}

	// Two green ci_watch stages: the stale one and the fresh one.
	stages, err := st.Pipeline.ListStages(ctx, run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	var ciSHAs []string
	var ciFences []float64
	mergeAttempts := 0
	for _, sr := range stages {
		switch sr.Stage {
		case "ci_watch":
			if sr.Outcome == nil || *sr.Outcome != store.StageOutcomeSuccess {
				continue
			}
			sha, _ := sr.Artifacts["ci_sha"].(string)
			ciSHAs = append(ciSHAs, sha)
			seq, _ := sr.Artifacts["ci_transition_seq"].(float64)
			ciFences = append(ciFences, seq)
		case "merge":
			mergeAttempts++
			if sr.Outcome != nil && *sr.Outcome == store.StageOutcomeSuccess {
				if got, _ := sr.Artifacts["merged_sha"].(string); got != e2eMergeCommit {
					t.Errorf("merged_sha artifact = %q, want %s", got, e2eMergeCommit)
				}
			}
		}
	}
	if len(ciSHAs) != 2 {
		t.Fatalf("successful ci_watch stages = %d (%v), want 2", len(ciSHAs), ciSHAs)
	}
	if ciSHAs[0] != e2eReviewedSHA || ciSHAs[1] != e2eSuccessorSHA {
		t.Errorf("ci_sha sequence = %v, want [%s %s]", ciSHAs, e2eReviewedSHA, e2eSuccessorSHA)
	}
	// The fence travels with the authorization: the stale verdict was issued
	// at 0, the fresh one at 1, and only the latter matches the ledger.
	if ciFences[0] != 0 || ciFences[1] != 1 {
		t.Errorf("ci_transition_seq sequence = %v, want [0 1]", ciFences)
	}
	if mergeAttempts != 2 {
		t.Errorf("merge stage attempts = %d, want 2 (one refused, one merged)", mergeAttempts)
	}
	if fake.branchDeletes != 0 {
		t.Errorf("branch deletes = %d, want 0 (merged MR source cleanup is left to RemoveSourceBranch)", fake.branchDeletes)
	}

	// Still exactly one ledger row: the re-gate produced no phantom movement.
	rows, err = st.MRHeadTransitions.ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("head transitions at completion = %d, want exactly 1", len(rows))
	}
	if fmt.Sprint(rows[0].State) != "ambiguous" {
		t.Errorf("state = %q, want ambiguous (an unrequested push is never attributable)", rows[0].State)
	}
}
