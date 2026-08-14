package agentcontext

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// newA2TestService builds a Service wired for A2: an in-memory engram store
// (memoryHierarchy) plus a PatternSvc, with both the builtin engrams and the
// builtin Go REST pattern seeded.
func newA2TestService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	// newEngramTestService sets LOOM_MCP_OUTPUT_FORMAT=json process-wide via a
	// sync.Once (os.Setenv, not t.Setenv) — do NOT add a t.Setenv here, whose
	// restore-to-unset cleanup would race that Once and break sibling tests.
	svc := newEngramTestService()
	svc.patterns = newTestPatternSvc()
	ctx := context.Background()
	svc.seedBuiltinEngrams(ctx)
	svc.patterns.SeedBuiltins(ctx)
	return svc, ctx
}

// writeStampedFixture creates the stable layout files a Go REST stamp produces,
// so the composed engrams' file_ref proofs resolve against this "checkout".
func writeStampedFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                    "module example.com/widget\n\ngo 1.22\n",
		"internal/config/config.go": "package config\n",
		"internal/server/server.go": "package server\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestSurfaceEngramCandidates: only slice templates that compose no engram are
// surfaced; a nil pattern yields nil.
func TestSurfaceEngramCandidates(t *testing.T) {
	t.Parallel()
	p := &Pattern{
		SliceTemplate: []PatternSliceTpl{
			{Name: "covered", Engrams: []string{"engram://x/y"}},
			{Name: "novel-a"},
			{Name: "novel-b", Engrams: []string{}},
		},
	}
	got := surfaceEngramCandidates(p)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, c := range got {
		names[c.SliceName] = true
	}
	if !names["novel-a"] || !names["novel-b"] {
		t.Fatalf("expected novel-a and novel-b, got %+v", got)
	}
	if surfaceEngramCandidates(nil) != nil {
		t.Fatalf("nil pattern should yield nil candidates")
	}
}

// TestGoRESTPattern_FullyDecomposed: the builtin pattern references exactly the
// builtin engram URIs (no drift), each URI parses, and the pattern surfaces no
// candidates — its single slice composes every engram (the goal state).
func TestGoRESTPattern_FullyDecomposed(t *testing.T) {
	t.Parallel()
	p := goRESTServicePattern()
	uris := goRESTEngramURIs()
	if len(uris) == 0 {
		t.Fatal("expected non-empty engram URIs")
	}
	if len(p.Engrams) != len(uris) {
		t.Fatalf("pattern.Engrams=%v want %v", p.Engrams, uris)
	}
	for i, u := range uris {
		if p.Engrams[i] != u {
			t.Fatalf("pattern.Engrams[%d]=%q want %q", i, p.Engrams[i], u)
		}
		if _, err := ParseEngramURI(u); err != nil {
			t.Fatalf("engram URI %q invalid: %v", u, err)
		}
	}
	if c := surfaceEngramCandidates(p); len(c) != 0 {
		t.Fatalf("Go REST pattern should surface no candidates, got %+v", c)
	}
}

// TestVerifyComposedEngrams_GreenStampUnlocks: a green stamp checkout verifies
// every composed engram and unlocks them for the instance repo.
func TestVerifyComposedEngrams_GreenStampUnlocks(t *testing.T) {
	svc, ctx := newA2TestService(t)
	root := writeStampedFixture(t)

	results := svc.verifyComposedEngrams(ctx, goRESTServicePattern(), "widget-svc", root)
	if len(results) != len(goRESTEngramURIs()) {
		t.Fatalf("got %d results, want %d", len(results), len(goRESTEngramURIs()))
	}
	for _, r := range results {
		if r.Status != "verified" {
			t.Fatalf("engram %s status=%s reason=%s; want verified", r.URI, r.Status, r.Reason)
		}
	}
	for _, uri := range goRESTEngramURIs() {
		item, err := svc.lookupEngramByURI(uri)
		if err != nil || item == nil {
			t.Fatalf("lookup %s: %v", uri, err)
		}
		if got := metadataString(item.Metadata, mdEngramProofStatus); got != ProofStatusVerified {
			t.Errorf("%s proof_status=%q want verified", uri, got)
		}
		if !contains(metadataStringSlice(item.Metadata, mdEngramUnlockedIn), "widget-svc") {
			t.Errorf("%s unlocked_in missing widget-svc: %v", uri, item.Metadata[mdEngramUnlockedIn])
		}
	}
}

// TestVerifyComposedEngrams_MissingCheckoutStaysUnverified: without the stamped
// files present (e.g. no live checkout), proofs go stale and nothing unlocks —
// the safe no-op path the deferred live wiring relies on.
func TestVerifyComposedEngrams_MissingCheckoutStaysUnverified(t *testing.T) {
	svc, ctx := newA2TestService(t)
	empty := t.TempDir()
	results := svc.verifyComposedEngrams(ctx, goRESTServicePattern(), "nope", empty)
	if len(results) != len(goRESTEngramURIs()) {
		t.Fatalf("got %d results, want %d", len(results), len(goRESTEngramURIs()))
	}
	for _, r := range results {
		if r.Status == "verified" {
			t.Fatalf("engram %s unexpectedly verified against an empty checkout", r.URI)
		}
	}
}

// TestRecordInstance_PopulatesEngrams: the green-stamp hook records the instance
// (taste gate) AND verifies the composed engrams (A2), leaving the catalog
// non-empty and the engrams unlocked for the instance repo.
func TestRecordInstance_PopulatesEngrams(t *testing.T) {
	svc, ctx := newA2TestService(t)
	root := writeStampedFixture(t)

	res, err := svc.HandlePatternRecordInstance(ctx, map[string]any{
		"pattern_id": "pattern-go-rest-service",
		"mr_ref":     "!900",
		"repo":       "widget-svc",
		"repo_root":  root,
	})
	got := okJSON(t, res, err)

	// Taste-gate fields preserved (already approved → count climbs, no promote).
	if got["instances_shipped_green"] != float64(1) {
		t.Fatalf("instances_shipped_green=%v want 1", got["instances_shipped_green"])
	}
	if got["status"] != "approved" {
		t.Fatalf("status=%v want approved", got["status"])
	}

	// A2: every composed engram verified, no candidates (fully decomposed).
	verified, _ := got["engrams_verified"].([]any)
	if len(verified) != len(goRESTEngramURIs()) {
		t.Fatalf("engrams_verified=%v want %d", got["engrams_verified"], len(goRESTEngramURIs()))
	}
	for _, v := range verified {
		m := v.(map[string]any)
		if m["status"] != "verified" {
			t.Fatalf("engram %v not verified: %v", m["uri"], m["reason"])
		}
	}
	if cands, _ := got["engram_candidates"].([]any); len(cands) != 0 {
		t.Fatalf("expected no candidates, got %v", got["engram_candidates"])
	}

	// The engram catalog is non-empty after the first green stamp.
	listRes, listErr := svc.HandleEngramList(ctx, map[string]any{})
	list := okJSON(t, listRes, listErr)
	if list["count"].(float64) < float64(len(goRESTEngramURIs())) {
		t.Fatalf("engram catalog count=%v; want >= %d", list["count"], len(goRESTEngramURIs()))
	}
}

// TestSeedBuiltinEngrams_Idempotent: re-seeding does not error or duplicate.
func TestSeedBuiltinEngrams_Idempotent(t *testing.T) {
	svc, ctx := newA2TestService(t) // seeds once
	svc.seedBuiltinEngrams(ctx)     // seed again — must be a no-op

	for _, uri := range goRESTEngramURIs() {
		item, err := svc.lookupEngramByURI(uri)
		if err != nil || item == nil {
			t.Fatalf("engram %s missing after re-seed: %v", uri, err)
		}
	}
}
