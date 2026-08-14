package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestClaimTransientRequeuePersistsAndCapsConcurrentClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mills.db")
	st, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	const cap = 3
	var wg sync.WaitGroup
	results := make(chan TransientRequeueClaim, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := st.ClaimTransientRequeue(ctx, "BACKLOG-1", cap)
			results <- claim
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	claimed := 0
	for result := range results {
		if result.Claimed {
			claimed++
		}
	}
	if claimed != cap {
		t.Fatalf("claimed = %d, want %d", claimed, cap)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claim, err := reopened.ClaimTransientRequeue(ctx, "BACKLOG-1", cap)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Claimed || claim.AttemptsUsed != cap {
		t.Fatalf("claim after reopen = %+v", claim)
	}
}

func TestClaimTransientRequeueRejectsInvalidInput(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tc := range []struct {
		id  string
		cap int
	}{{"", 1}, {"id", 0}, {"id", -1}} {
		if _, err := st.ClaimTransientRequeue(context.Background(), tc.id, tc.cap); err == nil {
			t.Fatalf("ClaimTransientRequeue(%q, %d) succeeded", tc.id, tc.cap)
		}
	}
}
