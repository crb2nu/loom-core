package crossrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

var (
	// ErrStampDeliveryDenied identifies a stamp rejected before its writer was
	// called, including missing/malformed identity and authorization denial.
	ErrStampDeliveryDenied = errors.New("crossrepo: stamp delivery denied")
	// ErrStampDeliveryFailed identifies an authorized stamp whose write failed.
	ErrStampDeliveryFailed = errors.New("crossrepo: stamp delivery failed")
)

// StampAuthorizer decides whether the caller may deliver to the exact project
// named by a stamp. Implementations must not substitute a home/default project.
type StampAuthorizer interface {
	AuthorizeStamp(context.Context, string) error
}

func validRouteProject(project string) bool {
	if project == "" || strings.ContainsAny(project, " \t\r\n") {
		return false
	}
	parts := strings.Split(project, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// StampDeliveryMetricSink is the bounded telemetry contract used by Deliverer.
type StampDeliveryMetricSink interface {
	RecordCrossRepoStampDelivery(outcome string)
}

// Deliverer is the authorization boundary around cross-repository stamp writes.
type Deliverer struct {
	Authorizer StampAuthorizer
	Writer     StampWriter
	Metrics    StampDeliveryMetricSink
}

// Deliver authorizes and persists a target-bound stamp. Validation and
// authorization happen before the writer is called. TargetProject is passed to
// the authorizer and writer unchanged; whitespace or malformed paths are
// rejected rather than normalized into a potentially different destination.
func (d *Deliverer) Deliver(ctx context.Context, stamp *store.Stamp) (err error) {
	outcome := telemetry.CrossRepoStampDeliveryFailure
	metrics := d.metricSink()
	defer func() { metrics.RecordCrossRepoStampDelivery(outcome) }()

	if d == nil {
		outcome = telemetry.CrossRepoStampDeliveryDenial
		return fmt.Errorf("%w: deliverer is nil", ErrStampDeliveryDenied)
	}
	if stamp == nil {
		outcome = telemetry.CrossRepoStampDeliveryDenial
		return fmt.Errorf("%w: stamp is required", ErrStampDeliveryDenied)
	}
	if !validRouteProject(stamp.TargetProject) {
		outcome = telemetry.CrossRepoStampDeliveryDenial
		return fmt.Errorf("%w: target project %q is missing or malformed", ErrStampDeliveryDenied, stamp.TargetProject)
	}
	if d.Authorizer == nil {
		outcome = telemetry.CrossRepoStampDeliveryDenial
		return fmt.Errorf("%w: authorizer is required", ErrStampDeliveryDenied)
	}
	if err := d.Authorizer.AuthorizeStamp(ctx, stamp.TargetProject); err != nil {
		outcome = telemetry.CrossRepoStampDeliveryDenial
		return fmt.Errorf("%w for target project %q: %w", ErrStampDeliveryDenied, stamp.TargetProject, err)
	}
	if d.Writer == nil {
		return fmt.Errorf("%w: stamp writer is required", ErrStampDeliveryFailed)
	}
	if err := d.Writer.Put(ctx, stamp); err != nil {
		return fmt.Errorf("%w for target project %q: %w", ErrStampDeliveryFailed, stamp.TargetProject, err)
	}
	outcome = telemetry.CrossRepoStampDeliverySuccess
	return nil
}

func (d *Deliverer) metricSink() StampDeliveryMetricSink {
	if d != nil && d.Metrics != nil {
		return d.Metrics
	}
	return telemetry.DefaultCrossRepoStampDeliveryMetrics()
}
