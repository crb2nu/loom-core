package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type councilDeadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline      time.Time
	writeDeadlineCalls int
}

func (r *councilDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.writeDeadline = deadline
	r.writeDeadlineCalls++
	return nil
}

func TestCouncilHandlersExtendWriteDeadline(t *testing.T) {
	const wantExtension = 10*time.Minute + 30*time.Second

	op := &operator{}
	tests := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{name: "run", path: "/api/mills/council/run", handler: op.handleCouncilRun},
		{name: "dryrun", path: "/api/mills/council/dryrun", handler: op.handleCouncilDryrun},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &councilDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			before := time.Now()
			tc.handler(recorder, httptest.NewRequest(http.MethodPost, tc.path, nil))
			after := time.Now()

			if recorder.writeDeadlineCalls != 1 {
				t.Fatalf("SetWriteDeadline calls = %d, want 1", recorder.writeDeadlineCalls)
			}
			if min, max := before.Add(wantExtension), after.Add(wantExtension); recorder.writeDeadline.Before(min) || recorder.writeDeadline.After(max) {
				t.Fatalf("write deadline = %s, want within [%s, %s]", recorder.writeDeadline, min, max)
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
		})
	}
}
