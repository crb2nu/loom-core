package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

type queuedProofDriver struct {
	operatorURL, adminToken, gitlabURL, gitlabToken, gitlabProject string
	client                                                         *http.Client
	poll                                                           time.Duration
	resume                                                         bool
}

type queuedProofRunDetail struct {
	Run struct {
		ID                   string `json:"ID"`
		BacklogID            string `json:"BacklogID"`
		State                string `json:"State"`
		CurrentStage         string `json:"CurrentStage"`
		MRIID                *int64 `json:"MRIID"`
		EscalationClass      string `json:"EscalationClass"`
		FailureClass         string `json:"FailureClass"`
		ExternalDependencyID string `json:"ExternalDependencyID"`
		ExternalDependency   string `json:"ExternalDependency"`
	} `json:"run"`
	Stages []struct {
		Stage     string         `json:"Stage"`
		Outcome   *string        `json:"Outcome"`
		Artifacts map[string]any `json:"Artifacts"`
		LogTail   string         `json:"LogTail"`
	} `json:"stages"`
}

type queuedProofStartResponse struct {
	RunID     string `json:"run_id"`
	BacklogID string `json:"backlog_id"`
	State     string `json:"state"`
	Decision  string `json:"decision"`
}

type queuedProofBacklog struct {
	ID            string
	State         string
	PlanID        string
	TargetProject string
	Labels        []string
	Policy        struct {
		AutoMerge bool `json:"auto_merge"`
	}
}

const (
	queuedProofVerdictPending            = "PENDING"
	queuedProofVerdictPass               = "PASS"
	queuedProofVerdictExternalDependency = "EXTERNAL_DEPENDENCY"
)

type queuedProofReport struct {
	workflow.QueuedProofEvidence
	DeclaredTarget string `json:"declared_target"`
	Verdict        string `json:"verdict"`
	Detail         string `json:"detail,omitempty"`
}

func runLiveQueuedProof(ctx context.Context, d queuedProofDriver, backlogID, planID, evidencePath string, timeout time.Duration) error {
	if strings.TrimSpace(d.adminToken) == "" || strings.TrimSpace(d.gitlabToken) == "" {
		return fmt.Errorf("live queued-proof requires --admin-token and --gitlab-token")
	}
	if timeout <= 0 || d.poll <= 0 {
		return fmt.Errorf("live queued-proof timeout and poll interval must be positive")
	}
	if strings.TrimSpace(d.gitlabProject) == "" {
		return fmt.Errorf("live queued-proof requires --queued-proof-target-project")
	}
	operatorURL := strings.TrimRight(d.operatorURL, "/")
	var persisted queuedProofBacklog
	if strings.TrimSpace(backlogID) == "" {
		if strings.TrimSpace(planID) == "" {
			return fmt.Errorf("live queued-proof requires --queued-proof-plan-id when no existing backlog id is supplied")
		}
		backlogID = fmt.Sprintf("KILLTEST-PATTERN-LOOM-%d", time.Now().UTC().UnixNano())
		seed := map[string]any{
			"ID": backlogID, "Title": "Pattern Loom queued-proof live kill-test",
			"Labels": []string{"pattern-loom", "queued-proof-killtest"},
			"PlanID": planID, "CreatedBy": "mills-workflow-killtest",
			"TargetProject": d.gitlabProject,
			"Policy":        map[string]any{"auto_merge": true},
		}
		body, err := json.Marshal(seed)
		if err != nil {
			return fmt.Errorf("encode queued Pattern Loom item: %w", err)
		}
		if err := d.request(ctx, http.MethodPost, operatorURL+"/api/mills/backlog", body, &persisted, true); err != nil {
			return fmt.Errorf("seed queued Pattern Loom item: %w", err)
		}
	} else if err := d.request(ctx, http.MethodGet, operatorURL+"/api/mills/backlog/"+url.PathEscape(backlogID), nil, &persisted, false); err != nil {
		return fmt.Errorf("read existing queued Pattern Loom item: %w", err)
	}
	if err := validateQueuedProofBacklog(persisted, backlogID, planID, d.gitlabProject); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var started queuedProofStartResponse
	report := queuedProofReport{DeclaredTarget: d.gitlabProject, Verdict: queuedProofVerdictPending}
	if d.resume {
		encoded, err := os.ReadFile(evidencePath)
		if err != nil {
			return fmt.Errorf("read queued-proof resume evidence: %w", err)
		}
		if err := json.Unmarshal(encoded, &report); err != nil {
			return fmt.Errorf("decode queued-proof resume evidence: %w", err)
		}
		if report.RunID == "" || report.BacklogID != backlogID || report.DeclaredTarget != d.gitlabProject || report.Verdict != queuedProofVerdictPending {
			return fmt.Errorf("queued-proof resume evidence contradicts backlog, target, or pending verdict")
		}
		started = queuedProofStartResponse{RunID: report.RunID, BacklogID: report.BacklogID, State: "queued"}
	} else {
		if err := d.request(ctx, http.MethodPost, operatorURL+"/api/mills/pipeline/runs/"+url.PathEscape(backlogID)+"/start", nil, &started, true); err != nil {
			return fmt.Errorf("start queued backlog item: %w", err)
		}
		if started.RunID == "" || started.BacklogID != backlogID || started.State != "queued" {
			return fmt.Errorf("start returned contradictory queue identity: run=%q backlog=%q state=%q", started.RunID, started.BacklogID, started.State)
		}
		report.QueuedProofEvidence = workflow.QueuedProofEvidence{BacklogID: backlogID, RunID: started.RunID, States: []workflow.QueuedProofState{{State: "queued", ObservedAt: time.Now().UTC()}}}
		if err := writeQueuedProofReport(evidencePath, report); err != nil {
			return err
		}
	}
	e := report.QueuedProofEvidence
	last := "queued"
	var detail queuedProofRunDetail
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("queued-proof timed out waiting for run %s", started.RunID)
		}
		if err := d.request(ctx, http.MethodGet, operatorURL+"/api/mills/pipeline/runs/"+url.PathEscape(started.RunID), nil, &detail, false); err != nil {
			return fmt.Errorf("observe run: %w", err)
		}
		state := detail.Run.State
		if state == "" || detail.Run.ID != started.RunID || detail.Run.BacklogID != backlogID {
			return fmt.Errorf("operator returned contradictory run identity")
		}
		if state != last {
			e.States = append(e.States, workflow.QueuedProofState{State: state, ObservedAt: time.Now().UTC()})
			last = state
		}
		quarantined := state == "quarantined" || detailQuarantined(detail)
		if runIsExternalDependencyIncident(detail) {
			report.QueuedProofEvidence = e
			report.CapturedAt = time.Now().UTC()
			report.Verdict = queuedProofVerdictExternalDependency
			report.Detail = fmt.Sprintf("run %s classified %s (dependency=%q id=%q)", started.RunID, pipeline.ClassificationExternalDependencyIncident, detail.Run.ExternalDependency, detail.Run.ExternalDependencyID)
			if err := writeQueuedProofReport(evidencePath, report); err != nil {
				return err
			}
			fmt.Printf("queued-proof EXTERNAL_DEPENDENCY — proof inconclusive without a false failure: %s\n", evidencePath)
			return nil
		}
		if quarantined || state == "done" || state == "escalated" || state == "paused" {
			e.Terminal = workflow.QueuedProofTerminal{State: state, Quarantined: quarantined}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
	project, iid, mrURL := terminalMR(detail, d.gitlabProject)
	e.MR.Project, e.MR.IID, e.MR.URL = project, iid, mrURL
	if project != d.gitlabProject {
		report.QueuedProofEvidence = e
		report.CapturedAt = time.Now().UTC()
		_ = writeQueuedProofReport(evidencePath, report)
		return fmt.Errorf("terminal MR project %q contradicts declared target %q", project, d.gitlabProject)
	}
	if iid > 0 && project != "" {
		var changes struct {
			State   string `json:"state"`
			Changes []struct {
				Diff string `json:"diff"`
			} `json:"changes"`
		}
		endpoint := strings.TrimRight(d.gitlabURL, "/") + "/projects/" + url.PathEscape(project) + "/merge_requests/" + strconv.FormatInt(iid, 10) + "/changes"
		for {
			if err := d.request(ctx, http.MethodGet, endpoint, nil, &changes, false); err != nil {
				e.CapturedAt = time.Now().UTC()
				report.QueuedProofEvidence = e
				_ = writeQueuedProofReport(evidencePath, report)
				return fmt.Errorf("read terminal MR diff: %w", err)
			}
			if changes.State == "merged" {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("queued-proof timed out waiting for MR %s!%d to auto-merge (state=%q)", project, iid, changes.State)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d.poll):
			}
		}
		e.MR.State, e.MR.ChangedFiles = changes.State, len(changes.Changes)
		for _, change := range changes.Changes {
			for _, line := range strings.Split(change.Diff, "\n") {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					e.MR.Additions++
				}
				if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					e.MR.Deletions++
				}
			}
		}
	}
	e.CapturedAt = time.Now().UTC()
	report.QueuedProofEvidence = e
	if err := workflow.AssertQueuedProof(e); err != nil {
		report.Detail = err.Error()
		_ = writeQueuedProofReport(evidencePath, report)
		return err
	}
	report.Verdict, report.Detail = queuedProofVerdictPass, ""
	if err := writeQueuedProofReport(evidencePath, report); err != nil {
		return err
	}
	fmt.Printf("queued-proof PASSED — one queued item reached terminal MR proof with %d changed files: %s\n", e.MR.ChangedFiles, evidencePath)
	return nil
}

func validateQueuedProofBacklog(item queuedProofBacklog, backlogID, planID, targetProject string) error {
	if item.ID != backlogID || item.State != "queued" || strings.TrimSpace(item.PlanID) == "" {
		return fmt.Errorf("queued-proof backlog identity is unsafe: id=%q state=%q plan=%q", item.ID, item.State, item.PlanID)
	}
	if planID != "" && item.PlanID != planID {
		return fmt.Errorf("queued-proof backlog plan %q contradicts requested plan %q", item.PlanID, planID)
	}
	if strings.TrimSpace(item.TargetProject) != strings.TrimSpace(targetProject) {
		return fmt.Errorf("queued-proof backlog target %q contradicts declared target %q", item.TargetProject, targetProject)
	}
	patternLoom := false
	for _, label := range item.Labels {
		if label == "pattern-loom" {
			patternLoom = true
			break
		}
	}
	if !patternLoom || !item.Policy.AutoMerge {
		return fmt.Errorf("queued-proof backlog must be Pattern Loom labeled with auto-merge enabled")
	}
	return nil
}

func writeQueuedProofReport(path string, report queuedProofReport) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode queued-proof report: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write queued-proof report: %w", err)
	}
	return nil
}

func (d queuedProofDriver) request(ctx context.Context, method, endpoint string, body []byte, out any, admin bool) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if admin {
		req.Header.Set("Authorization", "Bearer "+d.adminToken)
	} else if strings.HasPrefix(endpoint, strings.TrimRight(d.gitlabURL, "/")) {
		req.Header.Set("PRIVATE-TOKEN", d.gitlabToken)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %s", method, endpoint, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func runIsExternalDependencyIncident(d queuedProofRunDetail) bool {
	escalationClass := strings.TrimSpace(d.Run.EscalationClass)
	if strings.EqualFold(escalationClass, "external_dependency") || strings.EqualFold(escalationClass, string(pipeline.ClassificationExternalDependencyIncident)) {
		return true
	}
	if strings.TrimSpace(d.Run.ExternalDependencyID) != "" || strings.TrimSpace(d.Run.ExternalDependency) != "" {
		return true
	}
	for _, stage := range d.Stages {
		if strings.Contains(strings.ToLower(stage.LogTail), string(pipeline.ClassificationExternalDependencyIncident)) {
			return true
		}
	}
	return false
}

func detailQuarantined(d queuedProofRunDetail) bool {
	for _, s := range d.Stages {
		if strings.Contains(strings.ToLower(s.LogTail), "quarantin") {
			return true
		}
		if v, ok := s.Artifacts["quarantined"].(bool); ok && v {
			return true
		}
	}
	return false
}

func terminalMR(d queuedProofRunDetail, fallbackProject string) (string, int64, string) {
	project, mrURL := fallbackProject, ""
	var iid int64
	if d.Run.MRIID != nil {
		iid = *d.Run.MRIID
	}
	for _, s := range d.Stages {
		if v, ok := s.Artifacts["mr_url"].(string); ok && v != "" {
			mrURL = v
		}
		if v, ok := s.Artifacts["mr_project"].(string); ok && v != "" {
			project = v
		}
		if v, ok := s.Artifacts["mr_iid"].(float64); ok && v > 0 {
			iid = int64(v)
		}
	}
	return project, iid, mrURL
}
