package health

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fakeRemediationSource struct {
	incident *RemediationIncident
	mrs      []RemediationMR
}

func (f *fakeRemediationSource) LatestAdvisoryRed(context.Context) (*RemediationIncident, error) {
	return f.incident, nil
}
func (f *fakeRemediationSource) OpenRenovateMRs(context.Context) ([]RemediationMR, error) {
	return f.mrs, nil
}

type fakeRemediationSink struct {
	artifacts     map[string]bool
	creates, arms int
	labels        []string
	sha           string
}

func (f *fakeRemediationSink) ArtifactExists(_ context.Context, id string) (bool, error) {
	return f.artifacts[id], nil
}
func (f *fakeRemediationSink) PrioritizeAndArm(_ context.Context, mr RemediationMR, _ Advisory) error {
	f.arms++
	f.sha = mr.HeadSHA
	f.artifacts[Advisory{ID: "GO-2026-6303", Module: "golang.org/x/oauth2", FixedVersion: "0.30.0"}.Identity()] = true
	return nil
}
func (f *fakeRemediationSink) CreateBacklog(_ context.Context, id string, _ Advisory, labels []string) error {
	f.creates++
	f.labels = labels
	f.artifacts[id] = true
	return nil
}

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/govulncheck-job-248279.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseGovulncheckJob248279(t *testing.T) {
	got, err := ParseGovulncheck(strings.NewReader(string(fixture(t))))
	if err != nil {
		t.Fatal(err)
	}
	want := []Advisory{{ID: "GO-2026-6303", Module: "golang.org/x/oauth2", FixedVersion: "0.30.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseGovulncheckRejectsMalformedAndUnrelated(t *testing.T) {
	if _, err := ParseGovulncheck(strings.NewReader("{")); err == nil {
		t.Fatal("malformed report accepted")
	}
	got, err := ParseGovulncheck(strings.NewReader(`{"osv":{"id":"GO-X","affected":[]}}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestRemediatorFallbackIsIdempotentAndLabeled(t *testing.T) {
	src := &fakeRemediationSource{incident: &RemediationIncident{JobName: "security:govulncheck", Report: fixture(t)}}
	sink := &fakeRemediationSink{artifacts: map[string]bool{}}
	r := &Remediator{Source: src, Sink: sink}
	first, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || sink.creates != 1 {
		t.Fatalf("first=%+v second=%+v creates=%d", first, second, sink.creates)
	}
	if !reflect.DeepEqual(sink.labels, []string{RemediationLabel}) {
		t.Fatalf("labels=%v", sink.labels)
	}
}

func TestRemediatorRenovateArmsObservedHeadOnce(t *testing.T) {
	src := &fakeRemediationSource{incident: &RemediationIncident{JobName: "security:govulncheck", Report: fixture(t)}, mrs: []RemediationMR{{IID: 1763, Title: "Update module golang.org/x/oauth2 to v0.30.0", HeadSHA: "abc123"}}}
	sink := &fakeRemediationSink{artifacts: map[string]bool{}}
	r := &Remediator{Source: src, Sink: sink, RenovateEnabled: true}
	first, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Armed || second.Armed || sink.arms != 1 || sink.sha != "abc123" {
		t.Fatalf("first=%+v second=%+v arms=%d sha=%q", first, second, sink.arms, sink.sha)
	}
}

func TestRemediatorIgnoresOtherMainRed(t *testing.T) {
	src := &fakeRemediationSource{incident: &RemediationIncident{JobName: "test", Report: fixture(t)}}
	sink := &fakeRemediationSink{artifacts: map[string]bool{}}
	got, err := (&Remediator{Source: src, Sink: sink}).Tick(context.Background())
	if err != nil || got.Created || got.Armed || sink.creates+sink.arms != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestRemediatorRefusesUnpinnedRenovateMR(t *testing.T) {
	src := &fakeRemediationSource{incident: &RemediationIncident{JobName: "security:govulncheck", Report: fixture(t)}, mrs: []RemediationMR{{IID: 1, Title: "Update module golang.org/x/oauth2 to v0.30.0"}}}
	_, err := (&Remediator{Source: src, Sink: &fakeRemediationSink{artifacts: map[string]bool{}}, RenovateEnabled: true}).Tick(context.Background())
	if err == nil {
		t.Fatal("unpinned MR accepted")
	}
}
