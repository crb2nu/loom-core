package aimodels

import (
	"net/http"

	"github.com/crb2nu/loom/pkg/aimodels"
)

// Deps is the slice of *hud.App the aimodels domain needs. Decouples
// the domain from hud.App for testability.
type Deps interface {
	WriteJSON(w http.ResponseWriter, status int, v any)
	WriteError(w http.ResponseWriter, status int, msg string, err error)

	// AIModelsResolver returns the process-wide pkg/aimodels resolver
	// the daemon constructed at startup. May return nil before
	// initialization; handlers must guard.
	AIModelsResolver() *aimodels.Resolver

	// FlexInferProxyURL returns the base URL of the FlexInfer
	// OpenAI-compatible proxy (without /v1 suffix) the daemon was
	// configured with. Empty string disables the flexinfer/models
	// endpoint; the handler returns an empty list rather than 500.
	FlexInferProxyURL() string
}
