package mills

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestAppendEscalationTargetBindingResolvesHomeAndKeepsFirstWriter(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	run := &store.PipelineRun{ID: "PIPE-HOME-BINDING"}
	item := &store.BacklogItem{ID: "BL-HOME"}

	appended, err := AppendEscalationTargetBinding(ctx, env.store.Events, "pipeline", run, item, " services/loom-core ")
	if err != nil || !appended {
		t.Fatalf("first append=(%v, %v), want true, nil", appended, err)
	}
	appended, err = AppendEscalationTargetBinding(ctx, env.store.Events, "pipeline", run,
		&store.BacklogItem{ID: item.ID, TargetProject: "services/foreign"}, "services/loom-core")
	if err != nil || appended {
		t.Fatalf("second append=(%v, %v), want false, nil", appended, err)
	}
	project, found, err := escalationTargetBinding(ctx, env.store.Events, run.ID)
	if err != nil || !found || project != "services/loom-core" {
		t.Fatalf("binding=(%q, %v, %v), want resolved home project", project, found, err)
	}
}
