package daemon

import (
	"fmt"
	"sort"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/profiles"
)

// fullToolProfile is the profile the daemon publishes its aggregate cache
// under unless configured otherwise. It is the only built-in profile whose
// ceiling the daemon owns; the narrower built-ins (dev, k8s-ops, ...) keep
// their library defaults because their whole point is a small surface.
const fullToolProfile = "full"

// applyToolCeiling replaces the built-in "full" profile's tool cap with the
// daemon's configured ceiling. 0 lifts the cap entirely. The profile library
// ships "full" at 500, which silently discards the tail of the aggregate
// cache once the fleet grows past it (see ContextConfig.MaxTools).
func applyToolCeiling(mgr *profiles.Manager, maxTools int) {
	if mgr == nil {
		return
	}
	if maxTools < 0 {
		maxTools = 0
	}
	if full := mgr.Get(fullToolProfile); full != nil {
		full.MaxTools = maxTools
	}
}

// displacedServer is one server's share of the tools a ceiling cut.
type displacedServer struct {
	Name  string
	Tools int
	Total int // how many tools the server contributed before the cut
}

// displacedToolServers reports, per server, how many of all's tools are
// missing from kept. It diffs by name rather than assuming kept is a prefix
// of all, so it is right for the "full" profile (head truncation) and for
// server-filtered, priority-sorted profiles alike. Servers are ordered by
// tools lost (desc) then name, so the log line leads with the worst-hit
// server. A server whose Tools == Total lost its entire surface.
func displacedToolServers(all, kept []mcp.Tool) []displacedServer {
	if len(kept) >= len(all) {
		return nil
	}
	keptNames := make(map[string]struct{}, len(kept))
	for _, tool := range kept {
		keptNames[tool.Name] = struct{}{}
	}
	totals := make(map[string]int)
	lost := make(map[string]int)
	for _, tool := range all {
		server := toolServerName(tool.Name)
		totals[server]++
		if _, ok := keptNames[tool.Name]; !ok {
			lost[server]++
		}
	}
	if len(lost) == 0 {
		return nil
	}
	out := make([]displacedServer, 0, len(lost))
	for name, n := range lost {
		out = append(out, displacedServer{Name: name, Tools: n, Total: totals[name]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tools != out[j].Tools {
			return out[i].Tools > out[j].Tools
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// formatDisplacedServers renders the displacement report for a log field:
// "gitlab=36/36 weaver=12/12 zep=3/3" (lost/total per server).
func formatDisplacedServers(displaced []displacedServer) string {
	parts := make([]string, 0, len(displaced))
	for _, d := range displaced {
		parts = append(parts, fmt.Sprintf("%s=%d/%d", d.Name, d.Tools, d.Total))
	}
	return strings.Join(parts, " ")
}

// toolServerName extracts the server prefix from a namespaced tool name
// ("gitlab__get_pipeline" → "gitlab"). Un-namespaced names map to "".
func toolServerName(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		return name[:i]
	}
	return ""
}
