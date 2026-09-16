package spawn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func authTestController(t *testing.T, store Store) *K8sController {
	t.Helper()
	c := NewK8sController(nil, "", store, slog.Default())
	if err := c.RecoverFromStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}
func authTestSeed(t *testing.T, c *K8sController, id string) {
	t.Helper()
	c.UpdateState(t.Context(), &State{SpawnID: id, Status: StatusRunning, StartedAt: time.Now(), AuthMode: AuthModeClusterOAuth, AuthAccount: "a", Request: Request{AgentType: "claude-code"}})
}
func reserveAuth(c *K8sController, id string, at time.Time, limit int) (State, bool, error) {
	return c.RecordAuthAttempt(context.Background(), id, AuthFailure{Account: "a", Outcome: "oauth_rejected", At: at, ResetAt: at.Add(10 * time.Minute)}, AuthModeClusterAPIKey, "", true, limit)
}

func TestAuthCapConcurrentAndRestart(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := authTestController(t, store)
	for i := 0; i < 12; i++ {
		authTestSeed(t, c, fmt.Sprint(i))
	}
	var wg sync.WaitGroup
	var accepted atomic.Int32
	now := time.Now()
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, _ := reserveAuth(c, fmt.Sprint(i), now, 3)
			if ok {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("accepted=%d", accepted.Load())
	}
	fresh := authTestController(t, store)
	authTestSeed(t, fresh, "restart")
	if _, ok, err := reserveAuth(fresh, "restart", now, 3); err == nil || ok {
		t.Fatal("restart reset cap")
	}
	if _, ok, err := reserveAuth(fresh, "restart", now.UTC().Truncate(24*time.Hour).Add(24*time.Hour), 3); err != nil || !ok {
		t.Fatalf("day rollover: %v", err)
	}
}

func TestAuthReservationFailClosed(t *testing.T) {
	store := &toggleFailStore{}
	c := authTestController(t, store)
	authTestSeed(t, c, "a")
	store.fail = true
	store.err = errors.New("unavailable")
	if _, ok, err := reserveAuth(c, "a", time.Now(), 6); err == nil || ok {
		t.Fatal("failed persistence authorized launch")
	}
	s, _ := c.Get("a")
	if s.AuthFallbackAt != nil || s.AuthRetryPending {
		t.Fatal("failed reservation mutated state")
	}
	store.fail = false
	now := time.Now()
	s.StopRequestedAt = &now
	c.UpdateState(t.Context(), s)
	if _, ok, _ := reserveAuth(c, "a", now, 6); ok {
		t.Fatal("retry after stop")
	}
}

func TestAuthSharedStoreCap(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewK8sConfigMapStore(client, "test", "auth")
	c1 := authTestController(t, store)
	c2 := authTestController(t, store)
	authTestSeed(t, c1, "a")
	authTestSeed(t, c2, "b")
	now := time.Now()
	if _, ok, err := reserveAuth(c1, "a", now, 1); err != nil || !ok {
		t.Fatalf("reserve: %v", err)
	}
	if _, ok, err := reserveAuth(c2, "b", now, 1); err == nil || ok {
		t.Fatal("second controller bypassed cap")
	}
	// Bypass controller preflight: the store transaction must also enforce it.
	s, _ := c2.Get("b")
	s.AuthFallbackAt = &now
	s.AuthFallbackLimit = 1
	if err := store.Save(t.Context(), s); err == nil {
		t.Fatal("store accepted over-cap reservation")
	}
	rows, err := store.LoadAll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rows {
		if s.SpawnID == "a" {
			s.Status = StatusCompleted
			s.EndedAt = &now
			s.CleanupAt = &now
			if err := store.Save(t.Context(), s); err != nil {
				t.Fatal(err)
			}
		}
	}
	c3 := authTestController(t, store)
	if err := c3.Delete(t.Context(), "a"); err == nil {
		t.Fatal("deleted active daily reservation")
	}
	if c3.Prune(t.Context(), -time.Hour) != 0 {
		t.Fatal("pruned active reservation")
	}
}

func TestAuthFailureSnapshotAndRetention(t *testing.T) {
	c := authTestController(t, nil)
	authTestSeed(t, c, "a")
	now := time.Now()
	_, _, err := c.RecordAuthAttempt(t.Context(), "a", AuthFailure{Account: "a", Outcome: "oauth_rejected", At: now, ResetAt: now.Add(10 * time.Minute)}, AuthModeClusterOAuth, "a", false, 6)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.Get("a")
	s.AuthFailures[0].Account = "mutated"
	original, _ := c.Get("a")
	if original.AuthFailures[0].Account != "a" {
		t.Fatal("mutable failure slice escaped")
	}
	if !retainAuthState(original, now) || retainAuthState(original, now.Add(11*time.Minute)) {
		t.Fatal("exclusion retention wrong")
	}
}
