package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// CheckMergePermission reads the current token's permission on the actual MR,
// including target-branch protection. Public project visibility is not enough.
// Missing permission data remains an error, never an implicit grant or denial.
func (c *GitLabClient) CheckMergePermission(ctx context.Context, mrIID int64) (bool, error) {
	if c == nil || mrIID <= 0 {
		return false, errors.New("gitlab: client and positive MRIID required")
	}
	var mr struct {
		User struct {
			CanMerge *bool `json:"can_merge"`
		} `json:"user"`
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), mrIID)
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &mr); err != nil {
		if status, _ := GitLabHTTPStatus(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
			return false, nil
		}
		return false, err
	}
	if mr.User.CanMerge == nil {
		return false, errors.New("gitlab: merge request omitted user.can_merge")
	}
	return *mr.User.CanMerge, nil
}
