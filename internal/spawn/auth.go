package spawn

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// checkAuthCap enforces the UTC-day cross-mode reservation cap against every
// known state. A spawn that already holds a reservation passes unchanged, so
// re-saving a reserved row is idempotent. AuthFailure and retainAuthState live
// in types.go.
func checkAuthCap(states []*State, incoming *State) error {
	if incoming.AuthFallbackAt == nil {
		return nil
	}
	day := incoming.AuthFallbackAt.UTC().Truncate(24 * time.Hour)
	count := 0
	for _, s := range states {
		if s.SpawnID == incoming.SpawnID && s.AuthFallbackAt != nil {
			return nil
		}
		if s.AuthFallbackAt != nil && !s.AuthFallbackAt.Before(day) && s.AuthFallbackAt.Before(day.Add(24*time.Hour)) {
			count++
		}
	}
	if count >= incoming.AuthFallbackLimit {
		return fmt.Errorf("daily Claude auth fallback cap reached")
	}
	return nil
}

// checkAuthEntries is the shared-store half of the cap: only rows written by
// RecordAuthAttempt carry AuthFallbackLimit, so a reservation without one (a
// final-artifact write, an older HUD, a test fixture) is not re-judged here.
func checkAuthEntries(entries map[string]string, incoming *State) error {
	if incoming.AuthFallbackAt == nil || incoming.AuthFallbackLimit <= 0 {
		return nil
	}
	states := make([]*State, 0, len(entries))
	for _, raw := range entries {
		var s State
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return fmt.Errorf("read auth reservation: %w", err)
		}
		states = append(states, &s)
	}
	return checkAuthCap(states, incoming)
}

// RecordAuthAttempt serializes failure recording and reservation against all
// completions in this controller. Shared stores also check the ledger inside
// their resourceVersion transaction. Persistence failure never authorizes launch.
func (c *K8sController) RecordAuthAttempt(ctx context.Context, id string, failure AuthFailure, mode AuthMode, account string, retry bool, cap int) (State, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.spawns[id]
	if s == nil || IsTerminal(s.Status) || s.StopRequestedAt != nil {
		return State{}, false, nil
	}
	if s.AuthRetryPending {
		return *cloneStateForRead(s), false, nil
	}
	next := *cloneStateForRead(s)
	next.AuthOutcome = failure.Outcome
	next.AuthFailures = append(next.AuthFailures, failure)
	if err := c.persistLocked(ctx, &next); err != nil {
		return *s, false, err
	}
	*s = next
	if !retry {
		return *cloneStateForRead(s), false, nil
	}
	if mode != s.AuthMode {
		if s.AuthFallbackAt != nil {
			return *s, false, nil
		}
		now := failure.At
		next.AuthFallbackAt = &now
		next.AuthFallbackFrom = string(s.AuthMode)
		next.AuthFallbackLimit = cap
		var states []*State
		if c.store != nil {
			var err error
			states, err = c.store.LoadAll(ctx)
			if err != nil {
				return *s, false, err
			}
		} else {
			for _, row := range c.spawns {
				states = append(states, row)
			}
		}
		if err := checkAuthCap(states, &next); err != nil {
			return *s, false, err
		}
	}
	next.AuthMode, next.AuthAccount = mode, account
	next.AuthRetryPending = true
	if err := c.persistLocked(ctx, &next); err != nil {
		return *s, false, err
	}
	*s = next
	return *cloneStateForRead(s), retry, nil
}
