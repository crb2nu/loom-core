package mergequeue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExternalEnqueuer_RequiresPermissionCheck(t *testing.T) {
	st := newQueueStore(t)
	e := &ExternalEnqueuer{Store: st, Enabled: func() bool { return true }}
	result, err := e.Enqueue(context.Background(), ExternalCandidate{Producer: "mcp_gitlab", IdempotencyKey: "unauthorized", Project: "platform/gitops", MRIID: 718, SourceBranch: "fix/test", TargetBranch: "main", ObservedSHA: "head"})
	if err == nil || result.Entry != nil {
		t.Fatalf("unverified candidate admitted: result=%+v err=%v", result, err)
	}
	for _, table := range []string{"merge_queue", "backlog_items", "pipeline_runs"} {
		var count int
		if err := st.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s contains %d unverified rows", table, count)
		}
	}
}

func allowExternalMerge(context.Context, string, int64) (bool, error) { return true, nil }

func TestExternalEnqueuer_PermissionBeforeProvenance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lookupErr error
		want      error
	}{
		{"denied", nil, ErrMergePermissionDenied},
		{"unavailable", errors.New("GitLab unavailable"), ErrMergePermissionUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newQueueStore(t)
			e := &ExternalEnqueuer{Store: st, CheckPermission: func(ctx context.Context, project string, iid int64) (bool, error) {
				if project != "platform/gitops" || iid != 718 {
					t.Fatalf("wrong identity: %s !%d", project, iid)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
					t.Fatal("permission lookup is not bounded")
				}
				return false, tc.lookupErr
			}}
			result, err := e.Enqueue(context.Background(), ExternalCandidate{Producer: "mcp_gitlab", IdempotencyKey: "denied", Project: "platform/gitops", MRIID: 718, SourceBranch: "fix/test", TargetBranch: "main", ObservedSHA: "head"})
			if !errors.Is(err, tc.want) || result.Entry != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, table := range []string{"merge_queue", "backlog_items", "pipeline_runs"} {
				var count int
				if err := st.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("%s contains %d rejected rows", table, count)
				}
			}
		})
	}
}

func TestExternalEnqueuer_DisabledSkipsPermission(t *testing.T) {
	e := &ExternalEnqueuer{Store: newQueueStore(t), Enabled: func() bool { return false }, CheckPermission: func(context.Context, string, int64) (bool, error) {
		t.Fatal("disabled queue checked permission")
		return false, nil
	}}
	result, err := e.Enqueue(context.Background(), ExternalCandidate{Producer: "mcp_gitlab", IdempotencyKey: "disabled", Project: "p", MRIID: 1, SourceBranch: "x", TargetBranch: "main", ObservedSHA: "sha"})
	if err != nil || result.Outcome != "disabled" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
