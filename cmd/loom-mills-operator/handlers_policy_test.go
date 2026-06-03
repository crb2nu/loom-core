package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// gitopsPolicyFixture mirrors the real ConfigMap shape: a 4-space-indented
// top-level kill-switch line carrying the `# kill switch` comment, plus a
// nested `enabled:` key (squads) the handler must NOT touch.
const gitopsPolicyFixture = `apiVersion: v1
kind: ConfigMap
data:
  policy.yaml: |
    version: 2
    enabled: true    # kill switch; flip false to pause the reconciler

    squads:
      enabled: true    # nested - must stay untouched
`

type fakeGitOps struct {
	content   string
	getErr    error
	commitErr error
	mrErr     error

	committed bool
	commitReq clients.CreateCommitRequest
	mrReq     pipeline.CreateMRRequest
}

func (f *fakeGitOps) GetRawFile(_ context.Context, _, _ string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.content, nil
}

func (f *fakeGitOps) CreateCommit(_ context.Context, req clients.CreateCommitRequest) (clients.CreateCommitResponse, error) {
	f.committed = true
	f.commitReq = req
	if f.commitErr != nil {
		return clients.CreateCommitResponse{}, f.commitErr
	}
	return clients.CreateCommitResponse{ID: "deadbeef", WebURL: "https://gl/commit/deadbeef"}, nil
}

func (f *fakeGitOps) CreateMR(_ context.Context, req pipeline.CreateMRRequest) (pipeline.CreateMRResponse, error) {
	f.mrReq = req
	if f.mrErr != nil {
		return pipeline.CreateMRResponse{}, f.mrErr
	}
	return pipeline.CreateMRResponse{MRIID: 215, URL: "https://gl/mr/215"}, nil
}

func killSwitchCall(t *testing.T, op *operator, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/api/mills/policy/kill-switch", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/api/mills/policy/kill-switch", strings.NewReader(body))
	}
	op.handlePolicyKillSwitch(rec, r)
	return rec
}

func TestKillSwitch_PauseOpensMRAndFlipsOnlyTheSwitchLine(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	fake := &fakeGitOps{content: gitopsPolicyFixture}
	op.withKillSwitch(fake, "k3s/mills/configmap-policy.yaml", "main")

	rec := killSwitchCall(t, op, `{"action":"pause","reason":"deploy bad"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp killSwitchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Changed || resp.DesiredEnabled || !resp.PreviousEnabled {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if resp.MRURL != "https://gl/mr/215" || resp.MRIID != 215 {
		t.Errorf("MR fields not surfaced: %+v", resp)
	}
	if !fake.committed {
		t.Fatal("expected a commit")
	}
	got := fake.commitReq.Actions[0].Content
	if !strings.Contains(got, "enabled: false    # kill switch") {
		t.Errorf("kill-switch line not flipped to false:\n%s", got)
	}
	// The nested squads enabled must remain true.
	if !strings.Contains(got, "      enabled: true    # nested") {
		t.Errorf("nested squads enabled was wrongly modified:\n%s", got)
	}
	if fake.commitReq.StartBranch != "main" || !strings.HasPrefix(fake.commitReq.Branch, "mills/kill-switch-pause-") {
		t.Errorf("branch wiring wrong: %+v", fake.commitReq)
	}
	if !strings.Contains(fake.mrReq.Description, "deploy bad") {
		t.Errorf("reason not in MR description: %q", fake.mrReq.Description)
	}
}

func TestKillSwitch_ToggleFromEnabledPauses(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	fake := &fakeGitOps{content: gitopsPolicyFixture}
	op.withKillSwitch(fake, "", "") // exercise defaults

	rec := killSwitchCall(t, op, ``) // empty body => toggle
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp killSwitchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.DesiredEnabled {
		t.Errorf("toggle from enabled should target disabled, got %+v", resp)
	}
}

func TestKillSwitch_NoopWhenAlreadyInDesiredState(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	fake := &fakeGitOps{content: gitopsPolicyFixture}
	op.withKillSwitch(fake, "k3s/mills/configmap-policy.yaml", "main")

	// Policy is enabled:true; "resume" is a no-op.
	rec := killSwitchCall(t, op, `{"action":"resume"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp killSwitchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Changed {
		t.Errorf("expected no change, got %+v", resp)
	}
	if fake.committed {
		t.Error("no commit should be made for a no-op")
	}
}

func TestKillSwitch_Unconfigured503(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	// No withKillSwitch — gitopsClient stays nil.
	rec := killSwitchCall(t, op, `{"action":"pause"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func TestKillSwitch_MissingMarkerLineIs422(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	fake := &fakeGitOps{content: "version: 2\nenabled: true\n"} // no `# kill switch` comment
	op.withKillSwitch(fake, "k3s/mills/configmap-policy.yaml", "main")

	rec := killSwitchCall(t, op, `{"action":"pause"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if fake.committed {
		t.Error("must not commit when the marker line is missing")
	}
}

func TestKillSwitch_InvalidActionIs400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	fake := &fakeGitOps{content: gitopsPolicyFixture}
	op.withKillSwitch(fake, "k3s/mills/configmap-policy.yaml", "main")

	rec := killSwitchCall(t, op, `{"action":"explode"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestKillSwitch_CommitErrorIs502(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	fake := &fakeGitOps{content: gitopsPolicyFixture, commitErr: errors.New("boom")}
	op.withKillSwitch(fake, "k3s/mills/configmap-policy.yaml", "main")

	rec := killSwitchCall(t, op, `{"action":"pause"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body.String())
	}
}
