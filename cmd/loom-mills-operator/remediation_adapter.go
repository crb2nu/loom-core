package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/crb2nu/loom/pkg/mills/clients"
	millshealth "github.com/crb2nu/loom/pkg/mills/health"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type remediationSink struct {
	backlog *store.BacklogDAO
	gitlab  *clients.GitLabClient
}

func (s remediationSink) ArtifactExists(ctx context.Context, id string) (bool, error) {
	_, err := s.backlog.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
func (s remediationSink) PrioritizeAndArm(ctx context.Context, mr millshealth.RemediationMR, _ millshealth.Advisory) error {
	return s.gitlab.ArmAutoMerge(ctx, mr.IID, mr.HeadSHA)
}
func (s remediationSink) CreateBacklog(ctx context.Context, id string, a millshealth.Advisory, labels []string) error {
	title := fmt.Sprintf("Remediate %s: update %s to %s", a.ID, a.Module, a.FixedVersion)
	// The minted item must be dispatchable as-is: a spec the implementer can act
	// on and a slice the scope gate can enforce, otherwise the factory would plan
	// against an empty document.
	spec := fmt.Sprintf(`# %s

main is red on security:govulncheck for %s in module %s. Bump the module to at
least %s (go get %s@%s && go mod tidy), keep the change to go.mod/go.sum plus a
changelog fragment, and let the MR pipeline's security:govulncheck prove the fix.
Minted by the factory health plane (S5 advisory auto-remediation); identity %s.
`, title, a.ID, a.Module, a.FixedVersion, a.Module, a.FixedVersion, id)
	return s.backlog.Put(ctx, &store.BacklogItem{
		ID: id, Title: title, Labels: labels, State: store.BacklogQueued, Priority: store.P0,
		CreatedBy: "health-remediator", SpecDoc: spec,
		Slices:  []store.Slice{{Name: "remediation", Files: []string{"go.mod", "go.sum", "changelog.d/*.md"}, Tests: []string{"go build ./..."}}},
		Success: store.SuccessCriteria{Tests: []string{"go build ./..."}, ManualCheck: "security:govulncheck passes on the MR pipeline; the flagged advisory no longer appears."},
	})
}
