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
	grade = strings.ToLower(strings.TrimSpace(grade))
	if grade != "keep" && grade != "meh" && grade != "regret" {
		return nil, ErrInvalidGrade
	}
	if strings.ContainsAny(note, "\r\n") {
		return nil, ErrInvalidGradeNote
	}
	actor = strings.TrimSpace(actor)
	if runID == "" || actor == "" {
		return nil, fmt.Errorf("grade run: run ID and actor are required")
	}
	item, err := st.Backlog.GradeRun(ctx, runID, grade, strings.TrimSpace(note), actor, time.Now().UTC())
	if errors.Is(err, store.ErrBacklogNotGradable) {
		return nil, fmt.Errorf("%w: %v", ErrNotGradable, err)
	}
	return item, err
}
