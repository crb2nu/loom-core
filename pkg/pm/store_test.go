package pm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeQdrant is a minimal in-memory Qdrant HTTP server covering only the
// endpoints QdrantStore exercises: GET collection (vector size), PUT collection
// (create), PUT points (upsert), GET point, and POST scroll.
func fakeQdrant(t *testing.T) (*httptest.Server, *map[string]map[string]any) {
	t.Helper()
	collectionExists := false
	points := map[string]map[string]any{} // pointID -> payload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/collections/") &&
			!strings.Contains(path, "/points"):
			if !collectionExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":"collection not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{
					"config": map[string]any{
						"params": map[string]any{
							"vectors": map[string]any{"size": float64(VectorSize), "distance": "Cosine"},
						},
					},
				},
			})

		case r.Method == http.MethodPut && strings.HasPrefix(path, "/collections/") &&
			!strings.Contains(path, "/points"):
			collectionExists = true
			_ = json.NewEncoder(w).Encode(map[string]any{"result": true})

		case r.Method == http.MethodPut && strings.HasSuffix(strings.Split(path, "?")[0], "/points"):
			var body struct {
				Points []struct {
					ID      string         `json:"id"`
					Payload map[string]any `json:"payload"`
				} `json:"points"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, p := range body.Points {
				points[p.ID] = p.Payload
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})

		case r.Method == http.MethodGet && strings.Contains(path, "/points/"):
			id := path[strings.LastIndex(path, "/")+1:]
			payload, ok := points[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":"point not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"id": id, "payload": payload},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/points/scroll"):
			var pts []map[string]any
			for id, payload := range points {
				pts = append(pts, map[string]any{"id": id, "payload": payload})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"points": pts, "next_page_offset": nil},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &points
}

func TestQdrantStore_EndToEnd(t *testing.T) {
	srv, _ := fakeQdrant(t)
	cfg := Config{
		QdrantURL:      srv.URL,
		QdrantDistance: "Cosine",
		Collection:     RisksCollection,
	}
	store := NewQdrantStore(cfg)
	ctx := context.Background()

	if err := store.EnsureReady(ctx); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	// idempotent ensure (collection now exists path)
	if err := store.EnsureReady(ctx); err != nil {
		t.Fatalf("EnsureReady (exists): %v", err)
	}

	r := Risk{
		ID:      "risk-uuid-1",
		Project: "services/flexdeck",
		Title:   "Token revoked",
		Status:  "identified",
		Links:   []string{},
	}
	if err := store.Upsert(ctx, r, fallbackVector()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.Get(ctx, "risk-uuid-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Project != "services/flexdeck" || got.Title != "Token revoked" {
		t.Fatalf("Get returned wrong risk: %+v", got)
	}

	// missing point -> (nil, nil)
	missing, err := store.Get(ctx, "nope")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil for missing, got %+v", missing)
	}

	list, err := store.List(ctx, "services/flexdeck", "identified", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != "risk-uuid-1" {
		t.Fatalf("List returned %+v", list)
	}

	// non-matching filter still returns no error (client-side scroll returns
	// all; filter is server-side in prod, but here the fake ignores it — verify
	// no panic / error path)
	if _, err := store.List(ctx, "", "", 5); err != nil {
		t.Fatalf("List no-filter: %v", err)
	}
}
