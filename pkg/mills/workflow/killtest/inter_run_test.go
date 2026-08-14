package killtest

import (
	"strings"
	"testing"
	"time"
)

func TestValidateInterRunPodContinuity(t *testing.T) {
	previous := PreflightReport{
		Operator: PodIdentity{Name: "operator", UID: "operator-uid", ResourceVersion: "operator-pod-rv", PodCensusListResourceVersion: "operator-list-rv", ImageID: "operator@sha256:abc", StartedAt: time.Unix(1, 0)},
		Hud:      PodIdentity{Name: "hud", UID: "hud-uid", ResourceVersion: "hud-pod-rv", PodCensusListResourceVersion: "hud-list-rv", ImageID: "hud@sha256:def", StartedAt: time.Unix(2, 0)},
	}
	if err := ValidateInterRunPodContinuity(previous, previous); err != nil {
		t.Fatalf("unchanged identities rejected: %v", err)
	}

	next := previous
	next.Operator.PodCensusListResourceVersion = "next-operator-list-rv"
	next.Hud.PodCensusListResourceVersion = "next-hud-list-rv"
	if err := ValidateInterRunPodContinuity(previous, next); err != nil {
		t.Fatalf("observation-local Pod List resourceVersion drift rejected: %v", err)
	}

	next = previous
	next.Operator.ResourceVersion = "operator-pod-rv-drifted"
	if err := ValidateInterRunPodContinuity(previous, next); err == nil || !strings.Contains(err.Error(), "operator changed") {
		t.Fatalf("operator Pod resourceVersion drift error = %v", err)
	}

	next = previous
	next.Operator.UID = "operator-restarted"
	if err := ValidateInterRunPodContinuity(previous, next); err == nil || !strings.Contains(err.Error(), "operator changed") {
		t.Fatalf("operator restart error = %v", err)
	}

	next = previous
	next.Hud.StartedAt = next.Hud.StartedAt.Add(time.Second)
	if err := ValidateInterRunPodContinuity(previous, next); err == nil || !strings.Contains(err.Error(), "mobile-hud changed") {
		t.Fatalf("mobile-hud restart error = %v", err)
	}
}
