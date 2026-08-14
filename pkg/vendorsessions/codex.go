package vendorsessions

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexRecord is one line of a Codex rollout file.
type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexSessionMeta is the payload of the leading session_meta record.
// Source is a string for interactive sessions ("vscode", "exec") but an
// OBJECT for subagent threads ({"subagent":{"thread_spawn":{...}}}), so it
// decodes raw and resolves via sourceLabel.
type codexSessionMeta struct {
	ID         string          `json:"id"`
	CWD        string          `json:"cwd"`
	Originator string          `json:"originator"`
	Source     json.RawMessage `json:"source"`
	// ThreadSource is "user" for interactive chats, "automation" for
	// scheduled automation runs, "subagent" for spawned worker threads.
	ThreadSource string `json:"thread_source"`
}

// codexSubagentSource is the object form of session_meta.source for
// subagent threads.
type codexSubagentSource struct {
	Subagent struct {
		Other       string `json:"other"`
		ThreadSpawn struct {
			AgentNickname string `json:"agent_nickname"`
			AgentRole     string `json:"agent_role"`
		} `json:"thread_spawn"`
	} `json:"subagent"`
}

// sourceLabel renders session_meta.source as a display string: the plain
// string form as-is; the subagent object as "nickname · role" (or whatever
// subset exists).
func (m codexSessionMeta) sourceLabel() string {
	if s := rawString(m.Source); s != "" {
		return s
	}
	var sub codexSubagentSource
	if json.Unmarshal(m.Source, &sub) != nil {
		return ""
	}
	spawn := sub.Subagent.ThreadSpawn
	switch {
	case spawn.AgentNickname != "" && spawn.AgentRole != "":
		return spawn.AgentNickname + " · " + spawn.AgentRole
	case spawn.AgentNickname != "":
		return spawn.AgentNickname
	case sub.Subagent.Other != "":
		return sub.Subagent.Other
	}
	return ""
}

// listCodex enumerates ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
func listCodex(root string, opts ListOptions) []Session {
	if root == "" {
		return nil
	}
	if _, err := os.Stat(root); err != nil {
		return nil
	}

	var cands []candidate
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort walk: skip unreadable subtrees
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !opts.Since.IsZero() && info.ModTime().Before(opts.Since) {
			return nil
		}
		cands = append(cands, candidate{path: path, modTime: info.ModTime(), size: info.Size()})
		return nil
	})
	sortCandidates(cands)

	var sessions []Session
	for _, c := range cands {
		if len(sessions) >= opts.Limit {
			break
		}
		head := codexHeadMeta(c.path)
		if opts.CwdContains != "" && !strings.Contains(head.meta.CWD, opts.CwdContains) {
			continue
		}
		id := head.meta.ID
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(c.path), ".jsonl")
		}
		sourceLabel := head.meta.sourceLabel()
		source := head.meta.Originator
		if source == "" {
			source = sourceLabel
		}
		var kind string
		switch head.meta.ThreadSource {
		case "automation":
			kind = KindAutomation
		case "subagent":
			// Codex worker threads are the moral equivalent of Claude's
			// sidechains: machine-spawned side work, one shared kind.
			kind = KindSidechain
		}
		title := head.title
		if title == "" && kind == KindSidechain && sourceLabel != "" {
			// Subagent threads often carry no user_message at all; the
			// spawn descriptor ("Erdos the 2nd · explorer", "guardian")
			// is the best available handle.
			title = sourceLabel
		}
		sessions = append(sessions, Session{
			Vendor:     VendorCodex,
			ID:         id,
			Path:       c.path,
			CWD:        head.meta.CWD,
			Source:     source,
			Title:      title,
			Kind:       kind,
			StartedAt:  head.started,
			ModifiedAt: c.modTime,
			SizeBytes:  c.size,
		})
	}
	return sessions
}

// codexHeadInfo is everything the head scan of one rollout file yields.
type codexHeadInfo struct {
	meta    codexSessionMeta
	started time.Time
	title   string
}

// codexHeadMeta parses the leading session_meta record, then scans a few
// more lines for the first user message to title the session (automation
// runs open with "Automation: <name>", interactive chats with the user's
// prompt). Best-effort.
func codexHeadMeta(path string) codexHeadInfo {
	var info codexHeadInfo
	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	line, rerr := readLimitedLine(r, 1<<20)
	if len(line) == 0 {
		return info
	}
	var rec codexRecord
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" {
		return info
	}
	_ = json.Unmarshal(rec.Payload, &info.meta)
	if rec.Timestamp != "" {
		if t, terr := time.Parse(time.RFC3339Nano, rec.Timestamp); terr == nil {
			info.started = t
		}
	}
	for i := 0; i < 40 && rerr == nil; i++ {
		line, rerr = readLimitedLine(r, 1<<20)
		if len(line) == 0 {
			continue
		}
		if rec, ok := extractRawRecord(line); ok && rec.Role == "user" && titleWorthy(rec.Text) {
			info.title = titleFromText(rec.Text)
			break
		}
	}
	return info
}
