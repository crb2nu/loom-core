package internal

import "testing"

func TestClassifyCILog_RepresentativeClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		log       string
		wantClass string
		wantFree  bool
		wantTerm  bool
	}{
		{
			name:      "quota",
			log:       "job failed: 429 Too Many Requests from GitLab\n",
			wantClass: "transient_quota",
			wantFree:  true,
		},
		{
			name:      "infrastructure",
			log:       "image build failed: create buildah pod: pods build-1 already exists\n",
			wantClass: "infrastructure",
		},
		{
			name:      "configuration",
			log:       "merge failed: status 405 Method Not Allowed\n",
			wantClass: "configuration",
			wantTerm:  true,
		},
		{
			name:      "code default",
			log:       "go test ./...\n--- FAIL: TestWidget\n",
			wantClass: "code",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyCILog([]byte(tc.log))
			if got.Class != tc.wantClass {
				t.Fatalf("Class = %q, want %q", got.Class, tc.wantClass)
			}
			if got.FreeRetry != tc.wantFree {
				t.Fatalf("FreeRetry = %t, want %t", got.FreeRetry, tc.wantFree)
			}
			if got.Terminal != tc.wantTerm {
				t.Fatalf("Terminal = %t, want %t", got.Terminal, tc.wantTerm)
			}
			if got.Bytes != len(tc.log) {
				t.Fatalf("Bytes = %d, want %d", got.Bytes, len(tc.log))
			}
			if got.Lines == 0 {
				t.Fatal("Lines = 0, want non-zero")
			}
			if len(got.Evidence) == 0 {
				t.Fatal("Evidence is empty")
			}
		})
	}
}

func TestClassifyCILog_EmptyLogFailsClosed(t *testing.T) {
	t.Parallel()

	got := ClassifyCILog(nil)
	if got.Class != "code" {
		t.Fatalf("Class = %q, want code", got.Class)
	}
	if got.Lines != 0 || got.Bytes != 0 {
		t.Fatalf("input stats = %d bytes/%d lines, want zero", got.Bytes, got.Lines)
	}
	if got.Retryable != true {
		t.Fatalf("Retryable = %t, want true for code-class retry budget", got.Retryable)
	}
}
