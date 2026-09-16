package audit

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// supersessionFake scripts the four-call supersession surface and records the
// call order so tests can assert exactly where a run stops.
type supersessionFake struct {
	prior                                     int64
	found, commented, closed                  bool
	findErr, inspectErr, commentErr, closeErr error
	calls                                     []string
}

func (f *supersessionFake) FindPreviousOpenAuditDigest(_ context.Context, _ string) (int64, bool, error) {
	f.calls = append(f.calls, "find")
	if f.closed {
		return 0, false, f.findErr
	}
	return f.prior, f.found, f.findErr
}

func (f *supersessionFake) IssueHasComment(_ context.Context, _ int64, _ string) (bool, error) {
	f.calls = append(f.calls, "inspect")
	return f.commented, f.inspectErr
}

func (f *supersessionFake) CommentIssue(_ context.Context, _ int64, body string) error {
	f.calls = append(f.calls, "comment:"+body)
	if f.commentErr == nil {
		f.commented = true
	}
	return f.commentErr
}

func (f *supersessionFake) CloseIssue(_ context.Context, _ int64) error {
	f.calls = append(f.calls, "close")
	if f.closeErr == nil {
		f.closed = true
	}
	return f.closeErr
}

var _ DigestSupersessionIssuer = (*supersessionFake)(nil)

func TestSupersedePreviousDigest_CommentsThenCloses(t *testing.T) {
	f := &supersessionFake{prior: 41, found: true}
	if err := SupersedePreviousDigest(context.Background(), f, "2026-09-08", 42); err != nil {
		t.Fatal(err)
	}
	want := []string{"find", "inspect", "comment:superseded by #42", "close"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestSupersedePreviousDigest_NoPriorOrSelfIsNoOp(t *testing.T) {
	for _, f := range []*supersessionFake{{}, {prior: 42, found: true}} {
		if err := SupersedePreviousDigest(context.Background(), f, "2026-09-08", 42); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 {
			t.Fatalf("calls = %v, want lookup only", f.calls)
		}
	}
	f := &supersessionFake{prior: 41, found: true}
	if err := SupersedePreviousDigest(context.Background(), f, "2026-09-08", 0); err != nil || len(f.calls) != 0 {
		t.Fatalf("zero new iid must not look anything up: err=%v calls=%v", err, f.calls)
	}
	if err := SupersedePreviousDigest(context.Background(), nil, "2026-09-08", 42); err != nil {
		t.Fatalf("nil issuer must be a no-op: %v", err)
	}
}

func TestSupersedePreviousDigest_CloseRetryDoesNotDuplicateComment(t *testing.T) {
	f := &supersessionFake{prior: 41, found: true, closeErr: errors.New("close failed")}
	if err := SupersedePreviousDigest(context.Background(), f, "2026-09-08", 42); err == nil {
		t.Fatal("expected close error")
	}
	f.closeErr = nil
	if err := SupersedePreviousDigest(context.Background(), f, "2026-09-08", 42); err != nil {
		t.Fatal(err)
	}
	comments := 0
	for _, call := range f.calls {
		if call == "comment:superseded by #42" {
			comments++
		}
	}
	if comments != 1 {
		t.Fatalf("comment calls = %d, want 1 (%v)", comments, f.calls)
	}
	if err := SupersedePreviousDigest(context.Background(), f, "2026-09-08", 42); err != nil {
		t.Fatal(err)
	}
	if f.calls[len(f.calls)-1] != "find" {
		t.Fatalf("completed retry mutated closed issue: %v", f.calls)
	}
}

func TestSupersedePreviousDigest_StopsAtEachFailure(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		fake *supersessionFake
		want []string
	}{
		{"find", &supersessionFake{findErr: boom}, []string{"find"}},
		{"inspect", &supersessionFake{prior: 1, found: true, inspectErr: boom}, []string{"find", "inspect"}},
		{"comment", &supersessionFake{prior: 1, found: true, commentErr: boom}, []string{"find", "inspect", "comment:superseded by #2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := SupersedePreviousDigest(context.Background(), tc.fake, "2026-09-08", 2); err == nil {
				t.Fatal("expected error")
			}
			if !reflect.DeepEqual(tc.fake.calls, tc.want) {
				t.Fatalf("calls=%v want=%v", tc.fake.calls, tc.want)
			}
		})
	}
}
