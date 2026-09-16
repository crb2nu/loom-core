package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// detectNode checks for Node.js project indicators and populates the fingerprint.
func detectNode(projectDir string, fp *EnvFingerprint) {
	pkgPath := filepath.Join(projectDir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return
	}

	spec := LanguageSpec{
		Language:   "node",
		Version:    parseNodeVersion(string(data)),
		DepFile:    "package.json",
		DepManager: detectNodeDepManager(projectDir),
	}

	switch spec.DepManager {
	case "pnpm":
		if fileExists(filepath.Join(projectDir, "pnpm-lock.yaml")) {
			spec.LockFile = "pnpm-lock.yaml"
		}
	case "yarn":
		if fileExists(filepath.Join(projectDir, "yarn.lock")) {
			spec.LockFile = "yarn.lock"
		}
	default:
		if fileExists(filepath.Join(projectDir, "package-lock.json")) {
			spec.LockFile = "package-lock.json"
		}
	}

	fp.Languages = append(fp.Languages, spec)
}

// parseNodeVersion extracts a concrete Node.js version from the package.json
// engines field. engines.node holds a semver range (">=20.0.0", ">=18 <21",
// "^20.9.0", "20.x"), so the range is resolved to its floor — the lowest
// version satisfying the lower-bound clauses — rather than interpolated
// verbatim into an image tag. Returns "" when unset or unresolvable.
func parseNodeVersion(content string) string {
	var pkg struct {
		Engines map[string]string `json:"engines"`
	}
	if err := json.Unmarshal([]byte(content), &pkg); err != nil {
		return ""
	}
	v, ok := pkg.Engines["node"]
	if !ok {
		return ""
	}
	return resolveNodeVersion(v)
}

// resolveNodeVersion resolves a semver range to a single concrete version.
// Only the first ||-alternative is considered (alternatives are conventionally
// ordered oldest-first). The result keeps the precision the range author
// wrote: ">=18 <21" → "18", ">=20.0.0" → "20.0.0", "^20.9.0" → "20.9.0".
func resolveNodeVersion(rangeSpec string) string {
	first := strings.TrimSpace(strings.Split(rangeSpec, "||")[0])
	if first == "" {
		return ""
	}

	var floor []int
	for _, clause := range strings.FieldsFunc(first, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	}) {
		nums, ok := nodeClauseFloor(clause)
		if !ok {
			continue // upper bounds and exclusions contribute no floor
		}
		// Every lower bound must hold, so the range's floor is the max.
		if floor == nil || compareVersionNums(nums, floor) > 0 {
			floor = nums
		}
	}
	if floor == nil {
		return ""
	}

	parts := make([]string, len(floor))
	for i, n := range floor {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// nodeClauseFloor returns the lowest version a single range clause admits,
// or ok=false when the clause imposes no lower bound (<, <=, wildcards) or
// cannot be parsed.
func nodeClauseFloor(clause string) ([]int, bool) {
	clause = strings.TrimSpace(clause)
	op := ""
	for _, candidate := range []string{">=", "<=", ">", "<", "^", "~", "="} {
		if strings.HasPrefix(clause, candidate) {
			op = candidate
			clause = strings.TrimSpace(strings.TrimPrefix(clause, candidate))
			break
		}
	}
	switch op {
	case "<", "<=":
		return nil, false
	}

	var nums []int
	for _, seg := range strings.Split(clause, ".") {
		if seg == "x" || seg == "X" || seg == "*" {
			break // "18.x" floors at 18
		}
		n, err := strconv.Atoi(seg)
		if err != nil || n < 0 {
			return nil, false
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 || len(nums) > 3 {
		return nil, false
	}

	// node-semver reads ">18" as the next release after the 18 series
	// (>=19.0.0), so bump the last written segment.
	if op == ">" {
		nums[len(nums)-1]++
	}
	return nums, true
}

// compareVersionNums orders two version slices, padding missing segments
// with zero (<0, 0, >0).
func compareVersionNums(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av - bv
		}
	}
	return 0
}

// detectNodeDepManager determines which Node package manager is in use.
func detectNodeDepManager(projectDir string) string {
	if fileExists(filepath.Join(projectDir, "pnpm-lock.yaml")) {
		return "pnpm"
	}
	if fileExists(filepath.Join(projectDir, "yarn.lock")) {
		return "yarn"
	}
	return "npm"
}
