package agentcontext

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runSessionRetroAsync launches the session retrospective script in the
// background. The hook is intentionally fire-and-forget so it does not block
// session teardown or downstream hooks.
func (s *Service) runSessionRetroAsync(session *Session) {
	if session == nil {
		return
	}

	bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repoRoot, scriptPath, err := findRetroScript()
	if err != nil {
		s.logger.Warn("async session retro unavailable", "session_id", session.ID, "agent_id", session.AgentID, "error", err)
		return
	}

	cmd := exec.CommandContext(bg, "bash", scriptPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"LOOM_BINARY=loom",
		"AGENT_ID="+sessionAgentID(session, s.cfg.DefaultAgentID),
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn("async session retro failed",
			"session_id", session.ID,
			"agent_id", session.AgentID,
			"error", err,
			"output", strings.TrimSpace(string(output)),
		)
		return
	}

	s.logger.Info("async session retro queued",
		"session_id", session.ID,
		"agent_id", session.AgentID,
		"output", strings.TrimSpace(string(output)),
	)
}

func sessionAgentID(session *Session, fallback string) string {
	if session == nil {
		return fallback
	}
	if strings.TrimSpace(session.AgentID) != "" {
		return session.AgentID
	}
	return fallback
}

func findRetroScript() (string, string, error) {
	startDir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}

	dir := startDir
	for {
		scriptPath := filepath.Join(dir, "mcp", "skills", "session-retro", "scripts", "session-retro.sh")
		if st, statErr := os.Stat(scriptPath); statErr == nil && !st.IsDir() {
			return dir, scriptPath, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", "", fmt.Errorf("session-retro.sh not found from %s", startDir)
}
