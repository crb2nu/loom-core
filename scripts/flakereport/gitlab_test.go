package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGitLab is a minimal in-memory stand-in for the GitLab issues API —
// enough to exercise the create / dedup / bump paths end to end.
type fakeGitLab struct {
	t        *testing.T
	issues   []Issue
	labels   map[int]string // issue IID -> comma-joined labels
	comments map[int][]string
	updates  map[int]string
	created  []Issue
	nextIID  int
	failFind bool
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	return &fakeGitLab{
		t:        t,
		labels:   map[int]string{},
		comments: map[int][]string{},
		updates:  map[int]string{},
		nextIID:  100,
	}
}

// seed adds a pre-existing open issue.
func (f *fakeGitLab) seed(issue Issue, labels string) {
	issue.State = "opened"
	f.issues = append(f.issues, issue)
	f.labels[issue.IID] = labels
}

func (f *fakeGitLab) server() *httptest.Server {
	const base = "/api/v4/projects/42/issues"
	mux := http.NewServeMux()

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if f.failFind {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			label := r.URL.Query().Get("labels")
			search := r.URL.Query().Get("search")
			out := []Issue{}
			for _, issue := range f.issues {
				if issue.State != "opened" {
					continue
				}
				// GitLab matches labels EXACTLY, not by substring. Model that
				// faithfully so a label name that contains another as a
				// substring cannot pass here and fail in production.
				if label != "" && !hasExactLabel(f.labels[issue.IID], label) {
					continue
				}
				// GitLab's `search` is a fuzzy substring match. Modelling that
				// faithfully is what makes the exact-title assertion below a
				// real test rather than a tautology.
				if search != "" && !strings.Contains(issue.Title, search) {
					continue
				}
				out = append(out, issue)
			}
			writeJSON(w, out)

		case http.MethodPost:
			var body struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Labels      string `json:"labels"`
			}
			decodeBody(f.t, r, &body)
			issue := Issue{
				IID:         f.nextIID,
				Title:       body.Title,
				Description: body.Description,
				WebURL:      "https://gl/issues/" + strconv.Itoa(f.nextIID),
				State:       "opened",
			}
			f.nextIID++
			f.issues = append(f.issues, issue)
			f.labels[issue.IID] = body.Labels
			f.created = append(f.created, issue)
			writeJSON(w, issue)

		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base+"/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, base+"/")

		if iidStr, ok := strings.CutSuffix(rest, "/notes"); ok {
			iid, err := strconv.Atoi(iidStr)
			if err != nil {
				http.Error(w, "bad iid", http.StatusBadRequest)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			decodeBody(f.t, r, &body)
			f.comments[iid] = append(f.comments[iid], body.Body)
			writeJSON(w, map[string]any{"id": 1})
			return
		}

		iid, err := strconv.Atoi(rest)
		if err != nil {
			http.Error(w, "bad iid", http.StatusBadRequest)
			return
		}
		var body struct {
			Description string `json:"description"`
		}
		decodeBody(f.t, r, &body)
		f.updates[iid] = body.Description
		for i := range f.issues {
			if f.issues[i].IID == iid {
				f.issues[i].Description = body.Description
			}
		}
		writeJSON(w, map[string]any{"iid": iid})
	})

	return httptest.NewServer(mux)
}

// hasExactLabel reports whether a comma-joined label string contains `want` as
// a whole label.
func hasExactLabel(joined, want string) bool {
	for _, l := range strings.Split(joined, ",") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

func testCtx() Context {
	return Context{
		PipelineURL: "https://gl/pipelines/7",
		JobURL:      "https://gl/jobs/8",
		Ref:         "main",
		CommitSHA:   "deadbee",
		Now:         time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
}

func TestFileFlakeCreatesLabelledIssue(t *testing.T) {
	fake := newFakeGitLab(t)
	srv := fake.server()
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	o := Occurrence{Package: "pkg/spawn", Test: "TestRace", Runs: 2, Failures: 1}

	if err := fileFlake(context.Background(), gl, o, testCtx()); err != nil {
		t.Fatalf("fileFlake: %v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d issues, want 1", len(fake.created))
	}
	got := fake.created[0]
	if got.Title != "flake: TestRace" {
		t.Errorf("title = %q, want %q", got.Title, "flake: TestRace")
	}
	if lbl := fake.labels[got.IID]; !strings.Contains(lbl, FlakeLabel) {
		t.Errorf("labels = %q, want to contain %q", lbl, FlakeLabel)
	}
	if _, ok := ParseMarker(got.Description); !ok {
		t.Error("created issue has no dedup marker")
	}
}

// The dedup contract: repeat occurrences comment on the existing issue and
// bump the counter — they never file a duplicate.
func TestFileFlakeDedupsAndBumpsHits(t *testing.T) {
	fake := newFakeGitLab(t)
	srv := fake.server()
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	o := Occurrence{Package: "pkg/spawn", Test: "TestRace", Runs: 2, Failures: 1}

	for i := 0; i < 3; i++ {
		if err := fileFlake(context.Background(), gl, o, testCtx()); err != nil {
			t.Fatalf("fileFlake #%d: %v", i+1, err)
		}
	}

	if len(fake.created) != 1 {
		t.Fatalf("created %d issues across 3 occurrences, want 1", len(fake.created))
	}
	iid := fake.created[0].IID
	if got := len(fake.comments[iid]); got != 2 {
		t.Errorf("recurrence comments = %d, want 2", got)
	}
	state, ok := ParseMarker(fake.updates[iid])
	if !ok {
		t.Fatalf("updated description lost the marker: %q", fake.updates[iid])
	}
	if state.Hits != 3 {
		t.Errorf("Hits = %d after 3 occurrences, want 3", state.Hits)
	}
}

// GitLab's fuzzy `search` must not let "flake: TestFoo" adopt the issue that
// belongs to "flake: TestFooBar".
func TestFindOpenByTitleRequiresExactMatch(t *testing.T) {
	fake := newFakeGitLab(t)
	fake.seed(Issue{IID: 1, Title: "flake: TestFooBar"}, FlakeLabel)
	srv := fake.server()
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	got, err := gl.FindOpenByTitle(context.Background(), FlakeLabel, "flake: TestFoo")
	if err != nil {
		t.Fatalf("FindOpenByTitle: %v", err)
	}
	if got != nil {
		t.Fatalf("matched %q on an exact lookup for %q", got.Title, "flake: TestFoo")
	}
}

func TestFindOpenByTitleMatchesExisting(t *testing.T) {
	fake := newFakeGitLab(t)
	fake.seed(Issue{IID: 5, Title: "flake: TestFoo"}, FlakeLabel)
	srv := fake.server()
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	got, err := gl.FindOpenByTitle(context.Background(), FlakeLabel, "flake: TestFoo")
	if err != nil {
		t.Fatalf("FindOpenByTitle: %v", err)
	}
	if got == nil || got.IID != 5 {
		t.Fatalf("got %+v, want the seeded issue #5", got)
	}
}

// A dedup lookup outage must fail OPEN into a fresh issue rather than dropping
// the flake signal (same posture as the mills escalator).
func TestFileFlakeFailsOpenOnLookupError(t *testing.T) {
	fake := newFakeGitLab(t)
	fake.failFind = true
	srv := fake.server()
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	o := Occurrence{Package: "pkg/spawn", Test: "TestRace", Runs: 2, Failures: 1}

	if err := fileFlake(context.Background(), gl, o, testCtx()); err != nil {
		t.Fatalf("fileFlake should fail open, got error: %v", err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d issues, want 1 (fail-open)", len(fake.created))
	}
}

func TestGitLabSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	gl := NewGitLab(srv.URL+"/api/v4", "42", "tok")
	if _, err := gl.CreateIssue(context.Background(), "t", "d", []string{FlakeLabel}); err == nil {
		t.Fatal("expected an error on HTTP 403")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the status: %v", err)
	}
}
