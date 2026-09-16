package daemon

import (
	"fmt"
	"testing"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/profiles"
)

func namedTools(server string, n int) []mcp.Tool {
	tools := make([]mcp.Tool, 0, n)
	for i := 0; i < n; i++ {
		tools = append(tools, mcp.Tool{Name: fmt.Sprintf("%s__tool_%d", server, i)})
	}
	return tools
}

// The default configuration must publish the whole aggregate cache. The
// 2026-09-12 daemon held 589 tools and the library's built-in 500 ceiling
// dropped the last 89 before any proxy profile saw them.
func TestApplyToolCeiling_ZeroLiftsTheBuiltInFullCap(t *testing.T) {
	t.Parallel()
	mgr := profiles.NewManager()
	applyToolCeiling(mgr, 0)

	var all []mcp.Tool
	for i := 0; i < 60; i++ {
		all = append(all, namedTools(fmt.Sprintf("srv%02d", i), 10)...)
	}
	res := mgr.Filter(all, fullToolProfile)
	if res.Truncated {
		t.Fatalf("full profile truncated %d → %d with an unlimited ceiling", res.TotalBefore, res.TotalAfter)
	}
	if res.TotalAfter != len(all) {
		t.Fatalf("TotalAfter = %d, want %d", res.TotalAfter, len(all))
	}
}

func TestApplyToolCeiling_PositiveValueCapsFull(t *testing.T) {
	t.Parallel()
	mgr := profiles.NewManager()
	applyToolCeiling(mgr, 25)

	res := mgr.Filter(namedTools("a", 40), fullToolProfile)
	if !res.Truncated || res.TotalAfter != 25 {
		t.Fatalf("Filter = truncated:%v after:%d, want truncated:true after:25", res.Truncated, res.TotalAfter)
	}
	// Negative values are treated as "no cap" rather than trusted.
	applyToolCeiling(mgr, -5)
	if res := mgr.Filter(namedTools("a", 40), fullToolProfile); res.Truncated {
		t.Fatalf("negative ceiling must lift the cap, got truncated %d → %d", res.TotalBefore, res.TotalAfter)
	}
}

func TestApplyToolCeiling_LeavesNarrowProfilesAlone(t *testing.T) {
	t.Parallel()
	mgr := profiles.NewManager()
	before := mgr.Get("dev").MaxTools
	applyToolCeiling(mgr, 0)
	if got := mgr.Get("dev").MaxTools; got != before {
		t.Fatalf("dev profile MaxTools changed %d → %d", before, got)
	}
	applyToolCeiling(nil, 0) // must not panic
}

func TestDisplacedToolServers_ReportsTailByServer(t *testing.T) {
	t.Parallel()
	var all []mcp.Tool
	all = append(all, namedTools("git", 5)...)
	all = append(all, namedTools("gitlab", 4)...)
	all = append(all, namedTools("weaver", 3)...)
	all = append(all, mcp.Tool{Name: "bare_tool"})

	// Keep the first 7: git intact, gitlab loses 2 of 4, weaver and the
	// un-namespaced tool lose everything.
	got := displacedToolServers(all, all[:7])
	want := []displacedServer{
		{Name: "weaver", Tools: 3, Total: 3},
		{Name: "gitlab", Tools: 2, Total: 4},
		{Name: "", Tools: 1, Total: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("displaced = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("displaced[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s := formatDisplacedServers(got); s != "weaver=3/3 gitlab=2/4 =1/1" {
		t.Fatalf("formatDisplacedServers = %q", s)
	}
}

// A server-filtered profile keeps a non-prefix subset; the report must diff
// by name, not by position.
func TestDisplacedToolServers_NonPrefixKeptSet(t *testing.T) {
	t.Parallel()
	var all []mcp.Tool
	all = append(all, namedTools("git", 2)...)
	all = append(all, namedTools("gitlab", 2)...)
	all = append(all, namedTools("weaver", 2)...)

	kept := []mcp.Tool{all[4], all[5], all[0]} // weaver intact, one git, no gitlab
	got := displacedToolServers(all, kept)
	want := []displacedServer{
		{Name: "gitlab", Tools: 2, Total: 2},
		{Name: "git", Tools: 1, Total: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("displaced = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("displaced[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDisplacedToolServers_NothingLost(t *testing.T) {
	t.Parallel()
	all := namedTools("git", 3)
	if got := displacedToolServers(all, all); got != nil {
		t.Fatalf("kept == all: displaced = %+v, want nil", got)
	}
	if got := displacedToolServers(all, append(namedTools("git", 3), namedTools("x", 1)...)); got != nil {
		t.Fatalf("kept ⊇ all: displaced = %+v, want nil", got)
	}
	if got := displacedToolServers(nil, nil); got != nil {
		t.Fatalf("empty: displaced = %+v, want nil", got)
	}
	if got := displacedToolServers(all, nil); len(got) != 1 || got[0].Tools != 3 {
		t.Fatalf("nothing kept must count everything as lost, got %+v", got)
	}
}
