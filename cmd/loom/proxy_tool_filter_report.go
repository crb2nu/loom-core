package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// displacedReportSeen dedupes the displacement warning per distinct set so a
// client that re-lists tools every turn logs the eviction once, not once per
// turn. The proxy logs to stderr (the host CLI's MCP log); the daemon's own
// log never sees proxy-side shaping.
var (
	displacedReportMu   sync.Mutex
	displacedReportSeen = map[string]struct{}{}
)

// reportDisplacedTools warns once per distinct displaced set that the resolved
// profile's cap evicted required tools. Before this the eviction was silent:
// the tail of the priority list (ICC/PM, patterns, session bridge, mr-status)
// simply vanished from tools/list when a pattern was added without bumping
// proxyToolLimitLLM.
func reportDisplacedTools(agentHint, profile string, limit int, displaced []string) {
	if len(displaced) == 0 {
		return
	}
	key := strings.Join(displaced, ",")
	displacedReportMu.Lock()
	_, seen := displacedReportSeen[key]
	if !seen {
		displacedReportSeen[key] = struct{}{}
	}
	displacedReportMu.Unlock()
	if seen {
		return
	}
	resolvedProfile, _ := resolveProxyToolFilter(agentHint, profile, limit)
	fmt.Fprintf(os.Stderr, "loom proxy: tool profile %q (cap %d) displaced %d required tool(s): %s — raise the profile cap or trim its priority list\n",
		resolvedProfile, limit, len(displaced), strings.Join(displaced, ", "))
}
