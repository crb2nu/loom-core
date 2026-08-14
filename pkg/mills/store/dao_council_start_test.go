package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var councilClaimNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func councilClaimRequest(id string, estimate float64, limits CouncilStartLimits) ClaimCouncilStartRequest {
	return ClaimCouncilStartRequest{
		RunID:       id,
		Trigger:     CouncilTriggerCron,
		EstimateUSD: estimate,
		Limits:      limits,
		Now:         councilClaimNow,
		Notes:       "scheduled",
	}
}

func TestClaimCouncilStart_PersistsRunningRunAndReservation(t *testing.T) {
	st := newTestStore(t)
	claim, err := st.ClaimCouncilStart(context.Background(), councilClaimRequest(
		"COUNCIL-CLAIM-1", 5, CouncilStartLimits{MaxUSDPerRun: 5, MaxUSDPerDay: 50},
	))
	if err != nil {
		t.Fatalf("claim council start: %v", err)
	}
	if claim.Run.Outcome != CouncilOutcomeRunning || claim.Run.EndedAt != nil {
		t.Fatalf("provisional run = %+v, want running with no end", claim.Run)
	}
	if claim.Reservation.ReservedUSD != 5 || claim.Reservation.State != reservationStateActive {
		t.Fatalf("reservation = %+v, want active $5", claim.Reservation)
	}

	persisted, err := st.Council.Get(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatalf("get provisional run: %v", err)
	}
	if persisted.Outcome != CouncilOutcomeRunning || persisted.EndedAt != nil {
		t.Fatalf("persisted provisional run = %+v", persisted)
	}
}

func TestClaimCouncilStart_RejectsPerRunCapWithoutRows(t *testing.T) {
	st := newTestStore(t)
	_, err := st.ClaimCouncilStart(context.Background(), councilClaimRequest(
		"COUNCIL-TOO-EXPENSIVE", 5.01,
		CouncilStartLimits{MaxUSDPerRun: 5, MaxUSDPerDay: 50},
	))
	var exceeded *CouncilBudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("claim error = %v, want CouncilBudgetExceededError", err)
	}
	assertTableCount(t, st, "council_runs", 0)
	assertTableCount(t, st, "council_budget_reservations", 0)
}

func TestClaimCouncilStart_ConcurrentAdmissionsCannotOversubscribe(t *testing.T) {
	tests := []struct {
		name     string
		estimate float64
		limits   CouncilStartLimits
		want     int
	}{
		{name: "daily_usd", estimate: 1, limits: CouncilStartLimits{MaxUSDPerRun: 1, MaxUSDPerDay: 5}, want: 5},
		{name: "daily_runs", limits: CouncilStartLimits{MaxRunsPerDay: 4}, want: 4},
		{name: "active_runs", limits: CouncilStartLimits{MaxConcurrentRuns: 3}, want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			const contenders = 20
			start := make(chan struct{})
			errs := make(chan error, contenders)
			var wg sync.WaitGroup
			for i := 0; i < contenders; i++ {
				req := councilClaimRequest(fmt.Sprintf("COUNCIL-RACE-%s-%02d", tc.name, i), tc.estimate, tc.limits)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := st.ClaimCouncilStart(context.Background(), req)
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)

			successes, rejected := 0, 0
			for err := range errs {
				switch err {
				case nil:
					successes++
				default:
					var exceeded *CouncilBudgetExceededError
					if !errors.As(err, &exceeded) {
						t.Fatalf("unexpected claim error: %v", err)
					}
					rejected++
				}
			}
			if successes != tc.want || rejected != contenders-tc.want {
				t.Fatalf("admissions successes=%d rejected=%d, want %d/%d",
					successes, rejected, tc.want, contenders-tc.want)
			}
			assertTableCount(t, st, "council_runs", tc.want)
			assertTableCount(t, st, "council_budget_reservations", tc.want)
		})
	}
}

func TestFinalizeCouncilRun_ReplacesReservationWithFailedActualCost(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claim, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-FAILED-1", 1, CouncilStartLimits{MaxUSDPerRun: 1, MaxUSDPerDay: 1},
	))
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	ended := councilClaimNow.Add(time.Minute)
	claim.Run.Outcome = CouncilOutcomeError
	claim.Run.EndedAt = &ended
	claim.Run.CostFrontierUSD = 0.4
	claim.Run.Notes = "eval failed"
	if err := st.FinalizeCouncilRun(ctx, claim.Run); err != nil {
		t.Fatalf("finalize failed run: %v", err)
	}

	var active int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM council_budget_reservations WHERE state = 'active'`,
	).Scan(&active); err != nil {
		t.Fatalf("count active reservations: %v", err)
	}
	if active != 0 {
		t.Fatalf("active reservations = %d, want 0", active)
	}

	_, err = st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-AFTER-FAILED", 1, CouncilStartLimits{MaxUSDPerRun: 1, MaxUSDPerDay: 1},
	))
	var exceeded *CouncilBudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("second claim error = %v, want failed run actual cost to count", err)
	}
	if exceeded.SpentUSD != 0.4 || exceeded.ReservedUSD != 0 {
		t.Fatalf("budget snapshot spent=%v reserved=%v, want 0.4/0", exceeded.SpentUSD, exceeded.ReservedUSD)
	}
}

func TestFinalizeCouncilRun_PreservesClaimTimeIdentity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claim, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-IMMUTABLE-CLAIM", 1, CouncilStartLimits{MaxUSDPerDay: 5},
	))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ended := councilClaimNow.Add(time.Minute)
	detached := &CouncilRun{
		ID: claim.Run.ID, Trigger: CouncilTriggerManual,
		StartedAt: councilClaimNow.Add(-365 * 24 * time.Hour),
		EndedAt:   &ended, Outcome: CouncilOutcomeError, CostFrontierUSD: 0.4,
	}
	if err := st.FinalizeCouncilRun(ctx, detached); err != nil {
		t.Fatalf("finalize detached run: %v", err)
	}

	got, err := st.Council.Get(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("get finalized run: %v", err)
	}
	if got.Trigger != CouncilTriggerCron {
		t.Fatalf("trigger = %q, want immutable claim trigger %q", got.Trigger, CouncilTriggerCron)
	}
	if !got.StartedAt.Equal(councilClaimNow) {
		t.Fatalf("started_at = %v, want immutable claim time %v", got.StartedAt, councilClaimNow)
	}
	spent, err := st.Council.SumCostSince(ctx, councilClaimNow.Add(-time.Hour))
	if err != nil {
		t.Fatalf("sum cost: %v", err)
	}
	if spent != 0.4 {
		t.Fatalf("rolling-window spend = %v, want 0.4", spent)
	}
}

func TestCouncilPut_CannotTerminalizeActiveReservationManagedRun(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claim, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-PUT-ACTIVE", 1, CouncilStartLimits{},
	))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ended := councilClaimNow.Add(time.Minute)
	attempt := *claim.Run
	attempt.EndedAt = &ended
	attempt.Outcome = CouncilOutcomeSuccess
	attempt.CostFrontierUSD = 0.25
	if err := st.Council.Put(ctx, &attempt); !errors.Is(err, ErrCouncilRunAdmissionManaged) {
		t.Fatalf("Council.Put error = %v, want ErrCouncilRunAdmissionManaged", err)
	}

	got, err := st.Council.Get(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Outcome != CouncilOutcomeRunning || got.EndedAt != nil || got.CostFrontierUSD != 0 {
		t.Fatalf("active run was rewritten through Put: %+v", got)
	}
	var active int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM council_budget_reservations WHERE run_id = ? AND state = 'active'`, claim.Run.ID,
	).Scan(&active); err != nil {
		t.Fatalf("count active reservation: %v", err)
	}
	if active != 1 {
		t.Fatalf("active reservations = %d, want 1", active)
	}
}

func TestCouncilPut_CannotRewriteFinalizedReservationManagedRun(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claim, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-PUT-FINAL", 1, CouncilStartLimits{},
	))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ended := councilClaimNow.Add(time.Minute)
	claim.Run.EndedAt = &ended
	claim.Run.Outcome = CouncilOutcomeError
	claim.Run.CostFrontierUSD = 0.4
	if err := st.FinalizeCouncilRun(ctx, claim.Run); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	rewrite := *claim.Run
	rewrite.StartedAt = councilClaimNow.Add(-365 * 24 * time.Hour)
	rewrite.CostFrontierUSD = 0
	rewrite.Outcome = CouncilOutcomeSuccess
	if err := st.Council.Put(ctx, &rewrite); !errors.Is(err, ErrCouncilRunAdmissionManaged) {
		t.Fatalf("Council.Put error = %v, want ErrCouncilRunAdmissionManaged", err)
	}
	got, err := st.Council.Get(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Outcome != CouncilOutcomeError || got.CostFrontierUSD != 0.4 || !got.StartedAt.Equal(councilClaimNow) {
		t.Fatalf("finalized run was rewritten through Put: %+v", got)
	}
}

func TestClaimCouncilStart_ReservationInsertFailureRollsBackProvisionalRun(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_council_reservation_insert
		BEFORE INSERT ON council_budget_reservations
		BEGIN
			SELECT RAISE(ABORT, 'injected reservation insert failure');
		END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	_, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-ROLLBACK-CLAIM", 1, CouncilStartLimits{},
	))
	if err == nil {
		t.Fatal("claim unexpectedly succeeded")
	}
	assertTableCount(t, st, "council_runs", 0)
	assertTableCount(t, st, "council_budget_reservations", 0)
}

func TestFinalizeCouncilRun_ReservationReleaseFailureRollsBackTerminalUpdate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claim, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-ROLLBACK-FINALIZE", 1, CouncilStartLimits{},
	))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_council_reservation_release
		BEFORE UPDATE OF state ON council_budget_reservations
		WHEN OLD.state = 'active' AND NEW.state = 'released'
		BEGIN
			SELECT RAISE(ABORT, 'injected reservation release failure');
		END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	ended := councilClaimNow.Add(time.Minute)
	claim.Run.EndedAt = &ended
	claim.Run.Outcome = CouncilOutcomeError
	claim.Run.CostFrontierUSD = 0.4
	if err := st.FinalizeCouncilRun(ctx, claim.Run); err == nil {
		t.Fatal("finalize unexpectedly succeeded")
	}

	got, err := st.Council.Get(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Outcome != CouncilOutcomeRunning || got.EndedAt != nil || got.CostFrontierUSD != 0 {
		t.Fatalf("terminal update was not rolled back: %+v", got)
	}
	var state string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT state FROM council_budget_reservations WHERE run_id = ?`, claim.Run.ID,
	).Scan(&state); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if state != reservationStateActive {
		t.Fatalf("reservation state = %q, want active", state)
	}
}

func TestClaimCouncilStart_ReapsExpiredAdmissionConservatively(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	limits := CouncilStartLimits{MaxConcurrentRuns: 1}

	firstReq := councilClaimRequest("COUNCIL-STALE-1", 2.5, limits)
	firstReq.LeaseDuration = time.Minute
	first, err := st.ClaimCouncilStart(ctx, firstReq)
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if !first.Reservation.ExpiresAt.Equal(councilClaimNow.Add(time.Minute)) {
		t.Fatalf("reservation expiry = %v, want %v",
			first.Reservation.ExpiresAt, councilClaimNow.Add(time.Minute))
	}

	secondReq := councilClaimRequest("COUNCIL-AFTER-STALE", 1, limits)
	secondReq.Now = councilClaimNow.Add(2 * time.Minute)
	secondReq.LeaseDuration = time.Minute
	if _, err := st.ClaimCouncilStart(ctx, secondReq); err != nil {
		t.Fatalf("claim after expired admission: %v", err)
	}

	stale, err := st.Council.Get(ctx, first.Run.ID)
	if err != nil {
		t.Fatalf("get stale run: %v", err)
	}
	if stale.Outcome != CouncilOutcomeError || stale.EndedAt == nil {
		t.Fatalf("stale run = %+v, want terminal error", stale)
	}
	if got := stale.CostFrontierUSD + stale.CostLocalUSD; got != 2.5 {
		t.Fatalf("stale run cost = %v, want conservative reserved cost 2.5", got)
	}
	if !strings.Contains(stale.Notes, "reservation lease expired") {
		t.Fatalf("stale run notes = %q, want expiry reason", stale.Notes)
	}

	var state string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT state FROM council_budget_reservations WHERE run_id = ?`, first.Run.ID,
	).Scan(&state); err != nil {
		t.Fatalf("read stale reservation: %v", err)
	}
	if state != reservationStateReleased {
		t.Fatalf("stale reservation state = %q, want released", state)
	}
}

func TestFinalizeCouncilRun_DoesNotOverwriteReapedAdmission(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	limits := CouncilStartLimits{MaxConcurrentRuns: 1}

	firstReq := councilClaimRequest("COUNCIL-STALE-FINALIZE", 2.5, limits)
	firstReq.LeaseDuration = time.Minute
	first, err := st.ClaimCouncilStart(ctx, firstReq)
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	secondReq := councilClaimRequest("COUNCIL-REAPER", 1, limits)
	secondReq.Now = councilClaimNow.Add(2 * time.Minute)
	if _, err := st.ClaimCouncilStart(ctx, secondReq); err != nil {
		t.Fatalf("claim reaper: %v", err)
	}

	ended := councilClaimNow.Add(3 * time.Minute)
	first.Run.EndedAt = &ended
	first.Run.Outcome = CouncilOutcomeSuccess
	first.Run.CostFrontierUSD = 0.25
	if err := st.FinalizeCouncilRun(ctx, first.Run); !errors.Is(err, ErrCouncilAdmissionExpired) {
		t.Fatalf("finalize reaped run error = %v, want ErrCouncilAdmissionExpired", err)
	}

	got, err := st.Council.Get(ctx, first.Run.ID)
	if err != nil {
		t.Fatalf("get reaped run: %v", err)
	}
	if got.Outcome != CouncilOutcomeError || got.CostFrontierUSD != 2.5 {
		t.Fatalf("reaped run overwritten: %+v", got)
	}
}

func TestClaimCouncilStart_CommitsStaleRecoveryWhenSuccessorIsDenied(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	firstReq := councilClaimRequest("COUNCIL-STALE-DENIED", 2.5, CouncilStartLimits{})
	firstReq.LeaseDuration = time.Minute
	first, err := st.ClaimCouncilStart(ctx, firstReq)
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}

	secondReq := councilClaimRequest("COUNCIL-DENIED-AFTER-REAP", 1,
		CouncilStartLimits{MaxUSDPerDay: 3})
	secondReq.Now = councilClaimNow.Add(2 * time.Minute)
	_, err = st.ClaimCouncilStart(ctx, secondReq)
	var exceeded *CouncilBudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("successor error = %v, want CouncilBudgetExceededError", err)
	}
	if _, err := st.Council.Get(ctx, secondReq.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied provisional run lookup = %v, want not found", err)
	}

	stale, err := st.Council.Get(ctx, first.Run.ID)
	if err != nil {
		t.Fatalf("get stale run: %v", err)
	}
	if stale.Outcome != CouncilOutcomeError || stale.EndedAt == nil {
		t.Fatalf("stale recovery rolled back with denial: %+v", stale)
	}
	if got := stale.CostFrontierUSD + stale.CostLocalUSD; got != 2.5 {
		t.Fatalf("stale run cost = %v, want 2.5", got)
	}
}
