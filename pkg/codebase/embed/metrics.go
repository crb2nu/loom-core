package embed

import (
	"context"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

type requestObservation struct {
	ctx      context.Context
	provider string
	started  time.Time
}

func observeRequest(ctx context.Context, provider string) requestObservation {
	return requestObservation{ctx: ctx, provider: provider, started: time.Now()}
}

func (o requestObservation) finish(err error) {
	telemetry.RecordEmbedderRequest(o.ctx, o.provider, time.Since(o.started), err != nil)
}
