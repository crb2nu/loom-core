package pm

import (
	"testing"
	"time"
)

func TestIsValidLevelAndStatus(t *testing.T) {
	for _, ok := range []string{"low", "MEDIUM", " high "} {
		if !IsValidLevel(ok) {
			t.Errorf("expected %q valid level", ok)
		}
	}
	for _, bad := range []string{"", "huge", "none"} {
		if IsValidLevel(bad) {
			t.Errorf("expected %q invalid level", bad)
		}
	}
	for _, ok := range []string{"identified", "MITIGATING", " accepted ", "closed"} {
		if !IsValidStatus(ok) {
			t.Errorf("expected %q valid status", ok)
		}
	}
	for _, bad := range []string{"", "open", "done"} {
		if IsValidStatus(bad) {
			t.Errorf("expected %q invalid status", bad)
		}
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Nanosecond)
	in := Risk{
		ID:         "id-1",
		Project:    "services/flexdeck",
		Title:      "title",
		Likelihood: "high",
		Impact:     "low",
		Mitigation: "mitigate",
		Owner:      "owner",
		Status:     "mitigating",
		Links:      []string{"a", "b"},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	got := riskFromPayload(riskToPayload(in))
	if got.ID != in.ID || got.Project != in.Project || got.Title != in.Title ||
		got.Likelihood != in.Likelihood || got.Impact != in.Impact ||
		got.Mitigation != in.Mitigation || got.Owner != in.Owner || got.Status != in.Status {
		t.Fatalf("scalar mismatch: %+v vs %+v", got, in)
	}
	if len(got.Links) != 2 || got.Links[0] != "a" || got.Links[1] != "b" {
		t.Fatalf("links mismatch: %+v", got.Links)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) || !got.UpdatedAt.Equal(in.UpdatedAt) {
		t.Fatalf("time mismatch: %v/%v vs %v/%v", got.CreatedAt, got.UpdatedAt, in.CreatedAt, in.UpdatedAt)
	}
}

func TestRiskFromPayload_HandlesMissingAndWrongTypes(t *testing.T) {
	r := riskFromPayload(map[string]any{
		"id":         "x",
		"links":      []any{"one", 2, "three"}, // mixed; non-strings dropped
		"created_at": "not-a-time",
	})
	if r.ID != "x" {
		t.Fatalf("id = %q", r.ID)
	}
	if len(r.Links) != 2 {
		t.Fatalf("expected 2 string links, got %+v", r.Links)
	}
	if !r.CreatedAt.IsZero() {
		t.Fatalf("expected zero time for bad input, got %v", r.CreatedAt)
	}
}

func TestFallbackVector(t *testing.T) {
	v := fallbackVector()
	if len(v) != VectorSize {
		t.Fatalf("dim = %d, want %d", len(v), VectorSize)
	}
	if v[0] != 1 {
		t.Fatalf("first component = %v, want 1", v[0])
	}
	for i := 1; i < len(v); i++ {
		if v[i] != 0 {
			t.Fatalf("component %d non-zero", i)
		}
	}
}

func TestEmbedText(t *testing.T) {
	r := Risk{Title: "  hello ", Mitigation: " world "}
	if got := r.embedText(); got != "hello   world" {
		t.Fatalf("embedText = %q", got)
	}
	if (Risk{}).embedText() != "" {
		t.Fatal("empty risk should yield empty embed text")
	}
}

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	cfg := LoadConfigFromEnv()
	if cfg.Collection != RisksCollection {
		t.Fatalf("collection = %q, want %q", cfg.Collection, RisksCollection)
	}
	if cfg.QdrantDistance != "Cosine" {
		t.Fatalf("distance = %q, want Cosine", cfg.QdrantDistance)
	}
	if cfg.QdrantURL == "" {
		t.Fatal("qdrant url should default")
	}
	if cfg.EmbedTimeout <= 0 {
		t.Fatal("embed timeout should default > 0")
	}
}

func TestBuildEmbedder_Providers(t *testing.T) {
	hc := httpClient()
	for _, p := range []string{"morph", "flexinfer", "ollama", "dummy", "none", "unknown"} {
		cfg := LoadConfigFromEnv()
		cfg.EmbedProvider = p
		if e := buildEmbedder(hc, cfg); e == nil {
			t.Fatalf("buildEmbedder(%q) returned nil", p)
		}
	}
}
