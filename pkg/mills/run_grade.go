package mills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const BoltGradedEventKind = "bolt.graded"

var (
	ErrInvalidGrade     = errors.New("grade must be keep, meh, or regret")
	ErrInvalidGradeNote = errors.New("grade note must be one line")
	ErrNotGradable      = errors.New("only terminal work may be graded")
)

// GradeRun records a supervised taste signal for the work produced by runID.
func GradeRun(ctx context.Context, st *store.Store, runID, grade, note, actor string) (*store.BacklogItem, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("grade run: run ID and actor are required")
	}
	run, err := st.Pipeline.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return gradeItem(ctx, st, run.BacklogID, grade, note, actor, &runID)
}

// GradeItem records a supervised taste signal directly on terminal work.
func GradeItem(ctx context.Context, st *store.Store, itemID, grade, note, actor string) (*store.BacklogItem, error) {
	return gradeItem(ctx, st, itemID, grade, note, actor, nil)
}

func gradeItem(ctx context.Context, st *store.Store, itemID, grade, note, actor string, runID *string) (*store.BacklogItem, error) {
	grade = strings.ToLower(strings.TrimSpace(grade))
	if grade != "keep" && grade != "meh" && grade != "regret" {
		return nil, ErrInvalidGrade
	}
	if strings.ContainsAny(note, "\r\n") {
		return nil, ErrInvalidGradeNote
	}
	actor = strings.TrimSpace(actor)
	itemID = strings.TrimSpace(itemID)
	if itemID == "" || actor == "" {
		return nil, fmt.Errorf("grade item: item ID and actor are required")
	}
	item, err := st.Backlog.GradeItem(ctx, itemID, grade, strings.TrimSpace(note), actor, time.Now().UTC(), runID)
	if errors.Is(err, store.ErrBacklogNotGradable) {
		return nil, fmt.Errorf("%w: %v", ErrNotGradable, err)
	}
	return item, err
}
