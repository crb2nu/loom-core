package detect

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// detectPython checks for Python project indicators and populates the fingerprint.
func detectPython(projectDir string, fp *EnvFingerprint) {
	pyprojectPath := filepath.Join(projectDir, "pyproject.toml")
	data, err := os.ReadFile(pyprojectPath)
	if err != nil {
		// Fall back to requirements.txt
		if fileExists(filepath.Join(projectDir, "requirements.txt")) {
			spec := LanguageSpec{
				Language:   "python",
				DepFile:    "requirements.txt",
				DepManager: "pip",
			}
			fp.Languages = append(fp.Languages, spec)
		}
		return
	}

	spec := LanguageSpec{
		Language:   "python",
		Version:    parsePythonVersion(string(data)),
		DepFile:    "pyproject.toml",
		DepManager: detectPythonDepManager(projectDir),
	}

	switch spec.DepManager {
	case "uv":
		if fileExists(filepath.Join(projectDir, "uv.lock")) {
			spec.LockFile = "uv.lock"
		}
	case "poetry":
		if fileExists(filepath.Join(projectDir, "poetry.lock")) {
			spec.LockFile = "poetry.lock"
		}
	}

	fp.Languages = append(fp.Languages, spec)
}

// parsePythonVersion extracts a concrete python version from pyproject.toml.
// requires-python holds a PEP 440 specifier set (">=3.11", ">=3.11,<3.14",
// "~=3.11", "==3.12.*"), never a bare version, so the specifier is resolved
// to the lowest release series that satisfies every clause. Returns "" when
// no requires-python line exists or the specifier cannot be resolved; callers
// fall back to a default version.
func parsePythonVersion(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "requires-python") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) < 2 {
			continue
		}
		spec := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		return resolvePythonVersion(spec)
	}
	return ""
}

// pySpecClause is one comparison clause of a PEP 440 specifier set.
type pySpecClause struct {
	op       string
	major    int
	minor    int
	patch    int
	segments int  // version segments written in the clause (3.11 → 2)
	wildcard bool // version ended in .* (==3.12.*)
}

// resolvePythonVersion resolves a PEP 440 specifier set to the lowest
// "major.minor" series that satisfies every clause. Series granularity
// matches the python:X.Y image tags, which track the newest patch of a
// series. Returns "" for an empty, unparseable, or unsatisfiable specifier.
func resolvePythonVersion(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}

	var clauses []pySpecClause
	hasMinor := false
	for _, raw := range strings.Split(spec, ",") {
		clause, ok := parsePySpecClause(raw)
		if !ok {
			return ""
		}
		if clause.segments >= 2 || clause.wildcard {
			hasMinor = true
		}
		clauses = append(clauses, clause)
	}

	for major := 2; major <= 4; major++ {
		for minor := 0; minor <= 99; minor++ {
			satisfied := true
			for _, c := range clauses {
				if !c.allowsSeries(major, minor) {
					satisfied = false
					break
				}
			}
			if !satisfied {
				continue
			}
			// A major-only specifier like ">=3" should map to the plain
			// major tag (python:3), not a fabricated python:3.0.
			if !hasMinor {
				return strconv.Itoa(major)
			}
			return strconv.Itoa(major) + "." + strconv.Itoa(minor)
		}
	}
	return ""
}

// parsePySpecClause parses a single clause like ">=3.11" or "==3.12.*".
func parsePySpecClause(raw string) (pySpecClause, bool) {
	raw = strings.TrimSpace(raw)
	var clause pySpecClause
	for _, op := range []string{"~=", "==", "!=", ">=", "<=", ">", "<"} {
		if strings.HasPrefix(raw, op) {
			clause.op = op
			raw = strings.TrimSpace(strings.TrimPrefix(raw, op))
			break
		}
	}
	if clause.op == "" {
		// Bare versions are not valid requires-python, but tolerate them.
		clause.op = "=="
	}
	if strings.HasSuffix(raw, ".*") {
		clause.wildcard = true
		raw = strings.TrimSuffix(raw, ".*")
	}

	segs := strings.Split(raw, ".")
	if len(segs) < 1 || len(segs) > 3 {
		return clause, false
	}
	nums := make([]int, 0, 3)
	for _, s := range segs {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 0 {
			return clause, false
		}
		nums = append(nums, n)
	}
	clause.segments = len(nums)
	clause.major = nums[0]
	if len(nums) > 1 {
		clause.minor = nums[1]
	}
	if len(nums) > 2 {
		clause.patch = nums[2]
	}
	return clause, true
}

// allowsSeries reports whether some release in the major.minor series can
// satisfy the clause. Because the image tag tracks the series' newest patch,
// ordered comparisons run at minor granularity: ">=3.11.5" admits the 3.11
// series, and ">" is treated like ">=" (the newest 3.11.x exceeds 3.11).
func (c pySpecClause) allowsSeries(major, minor int) bool {
	cmp := compareSeries(major, minor, c.major, c.minor)
	switch c.op {
	case ">=", ">":
		return cmp >= 0
	case "<":
		if cmp != 0 {
			return cmp < 0
		}
		// Same series: X.Y.0 still satisfies "<X.Y.Z" when Z > 0.
		return c.segments == 3 && c.patch > 0
	case "<=":
		return cmp <= 0
	case "==":
		return cmp == 0
	case "!=":
		if c.segments == 3 && !c.wildcard {
			// A patch-level exclusion cannot rule out a whole series.
			return true
		}
		return cmp != 0
	case "~=":
		// "~=3.11" → >=3.11,<4.0. "~=3.11.2" → >=3.11.2,==3.11.*.
		switch c.segments {
		case 2:
			return major == c.major && minor >= c.minor
		case 3:
			return cmp == 0
		}
		return false
	}
	return false
}

// compareSeries orders two major.minor series (<0, 0, >0).
func compareSeries(aMajor, aMinor, bMajor, bMinor int) int {
	if aMajor != bMajor {
		return aMajor - bMajor
	}
	return aMinor - bMinor
}

// detectPythonDepManager determines which Python dependency manager is in use.
func detectPythonDepManager(projectDir string) string {
	if fileExists(filepath.Join(projectDir, "uv.lock")) {
		return "uv"
	}
	if fileExists(filepath.Join(projectDir, "poetry.lock")) {
		return "poetry"
	}
	// Check pyproject.toml for [tool.poetry] section
	data, err := os.ReadFile(filepath.Join(projectDir, "pyproject.toml"))
	if err == nil && strings.Contains(string(data), "[tool.poetry]") {
		return "poetry"
	}
	return "uv"
}
