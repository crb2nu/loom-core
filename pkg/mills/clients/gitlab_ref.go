package clients

import (
	"net/url"
	"strconv"
	"strings"
)

// GitLabMRReference is the identity carried by an MR reference. Bare numeric
// and !IID references are scoped by their caller; canonical GitLab URL refs
// additionally bind the authority and full project path.
type GitLabMRReference struct {
	IID          int64
	Project      string
	Authority    string
	ProjectBound bool
}

// ParseGitLabMRReference parses the free-form references emitted by plan and
// legacy pipeline artifacts. URL paths are decoded exactly once by net/url;
// callers must compare Project without applying a second PathUnescape.
func ParseGitLabMRReference(ref string) (GitLabMRReference, bool) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return GitLabMRReference{}, false
	}

	parsed := GitLabMRReference{}
	const mrPathMarker = "/-/merge_requests/"
	if strings.Contains(s, mrPathMarker) {
		u, err := url.Parse(s)
		if err != nil {
			return GitLabMRReference{}, false
		}
		if u.IsAbs() || u.Host != "" {
			parsed.Authority = CanonicalGitLabAuthority(s)
			if parsed.Authority == "" {
				return GitLabMRReference{}, false
			}
		}
		idx := strings.LastIndex(u.Path, mrPathMarker)
		if idx < 0 {
			return GitLabMRReference{}, false
		}
		parsed.Project = strings.Trim(u.Path[:idx], "/")
		if parsed.Project == "" {
			return GitLabMRReference{}, false
		}
		s = u.Path[idx+len(mrPathMarker):]
		if cut := strings.IndexByte(s, '/'); cut >= 0 {
			s = s[:cut]
		}
		parsed.ProjectBound = true
	}

	s = strings.TrimPrefix(s, "!")
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 || (parsed.ProjectBound && end != len(s)) {
		return GitLabMRReference{}, false
	}
	iid, err := strconv.ParseInt(s[:end], 10, 64)
	if err != nil || iid <= 0 {
		return GitLabMRReference{}, false
	}
	parsed.IID = iid
	return parsed, true
}

// CanonicalGitLabProject trims only presentation separators from a GitLab
// project path. GitLab project paths are authorization identities, so case and
// a trailing .git suffix remain significant. It deliberately does not
// URL-decode a path input: URL paths returned by net/url have already been
// decoded once.
func CanonicalGitLabProject(project string) string {
	p := strings.TrimSpace(project)
	if u, err := url.Parse(p); err == nil && u.Scheme != "" && u.Host != "" {
		p = u.Path
	}
	return strings.Trim(p, "/")
}

func SameGitLabProject(a, b string) bool {
	ca, cb := CanonicalGitLabProject(a), CanonicalGitLabProject(b)
	return ca != "" && ca == cb
}

// CanonicalGitLabAuthority returns the lower-cased host[:port] of an absolute
// http(s) URL. Credentials and non-http schemes are rejected.
func CanonicalGitLabAuthority(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

func SameGitLabAuthority(authority, baseURL string) bool {
	return authority != "" && authority == CanonicalGitLabAuthority(baseURL)
}
