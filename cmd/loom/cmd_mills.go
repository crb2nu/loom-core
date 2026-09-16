// cmd_mills.go implements `loom mills` subcommands. The mills lives in-cluster
// (cmd/loom-mills-operator running on k3s), so these commands are pure HTTP
// clients — there is no socket fallback because the canonical store and
// reconciler are never local. The Mac CLI authenticates with an admin token
// when one is configured; for the initial slice (1.2 stub) the operator
// returns the status payload without auth.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// defaultMillsOperatorURL is the public ingress, which reaches the in-cluster
// operator through the cloudflared tunnel both on- and off-LAN (the operator
// Service is ClusterIP-only, so there is no LAN-direct path from the Mac). The
// host is gated by Cloudflare Access at the edge, so the CLI injects a service
// token (see millsCFAccessHeaders). Override with LOOM_MILLS_OPERATOR_URL or
// --operator-url=http://localhost:8090 for `kubectl port-forward` workflows.
const defaultMillsOperatorURL = "https://mills.flexinfer.ai"

// newMillsCmd returns the `loom mills` command group.
func newMillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mills",
		Short: "Loom Mills control plane (cluster operator: council + pipeline)",
		Long: `Talk to the in-cluster loom-mills-operator (council scheduler + pipeline reconciler).

The operator is single-source-of-truth for Loom Mills state. Configure the URL
via LOOM_MILLS_OPERATOR_URL (default: ` + defaultMillsOperatorURL + `).
Set LOOM_MILLS_TOKEN for admin-token-gated endpoints once they ship.`,
	}
	cmd.PersistentFlags().String("operator-url", "", "Operator base URL (default: $LOOM_MILLS_OPERATOR_URL or "+defaultMillsOperatorURL+")")
	cmd.PersistentFlags().Duration("timeout", 10*time.Second, "Per-request timeout")
	cmd.PersistentFlags().Bool("json", false, "Emit raw JSON instead of the human-readable summary")

	cmd.AddCommand(
		newMillsStatusCmd(),
		newMillsCouncilCmd(),
		newMillsBacklogCmd(),
		newMillsEvalCmd(),
		newMillsPipelinesCmd(),
		newMillsSquadsCmd(),
		newMillsPatternsCmd(),
		newMillsStampCmd(),
		newMillsAuditCmd(),
	)
	return cmd
}

// millsClient resolves the operator URL + admin token from flags/env and
// returns an HTTP client tuned for the mills surface.
type millsClient struct {
	baseURL  string
	token    string
	cfID     string
	cfSecret string
	http     *http.Client
}

func resolveMillsClient(cmd *cobra.Command) (*millsClient, error) {
	urlFlag, _ := cmd.Flags().GetString("operator-url")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	base := strings.TrimSpace(urlFlag)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("LOOM_MILLS_OPERATOR_URL"))
	}
	if base == "" {
		base = defaultMillsOperatorURL
	}
	base = strings.TrimRight(base, "/")
	cfID, cfSecret := millsCFAccessHeaders()
	return &millsClient{
		baseURL:  base,
		token:    strings.TrimSpace(os.Getenv("LOOM_MILLS_TOKEN")),
		cfID:     cfID,
		cfSecret: cfSecret,
		http:     &http.Client{Timeout: timeout},
	}, nil
}

// millsCFAccessHeaders resolves the Cloudflare Access service-token credentials
// used to pass the edge gate on the public operator ingress.
//
// Priority: LOOM_MILLS_CF_ACCESS_ID/SECRET > CF_ACCESS_CLIENT_ID/SECRET env >
// the shared loom config (~/.config/loom/config.yaml hub.cf_access_*, via
// loadHUDConfig). The same Cloudflare Access application gates every
// *.flexinfer.ai host, so the hub service token also authenticates mills.
func millsCFAccessHeaders() (cfID, cfSecret string) {
	if id := strings.TrimSpace(os.Getenv("LOOM_MILLS_CF_ACCESS_ID")); id != "" {
		return id, strings.TrimSpace(os.Getenv("LOOM_MILLS_CF_ACCESS_SECRET"))
	}
	if id := strings.TrimSpace(os.Getenv("CF_ACCESS_CLIENT_ID")); id != "" {
		return id, strings.TrimSpace(os.Getenv("CF_ACCESS_CLIENT_SECRET"))
	}
	_, configID, configSecret := loadHUDConfig()
	return configID, configSecret
}

// millsResponseBodyCap bounds one operator response body. 64 MiB is ~50×
// today's full backlog ledger; a body past it is a runaway, not a big list.
const millsResponseBodyCap = 64 << 20

// get performs an authenticated GET against path (relative to the operator
// base URL) and decodes the JSON body into out.
func (c *millsClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// post sends a JSON body. body=nil sends an empty request — mirrors how
// the operator parses missing bodies as a zero councilRunRequest.
func (c *millsClient) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *millsClient) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// Pass the Cloudflare Access edge gate on the public ingress. Skipped for
	// loopback targets (port-forward) so the service token never leaks to a
	// local endpoint.
	if c.cfID != "" && c.cfSecret != "" && !isLocalHUDURL(c.baseURL) {
		req.Header.Set("CF-Access-Client-Id", c.cfID)
		req.Header.Set("CF-Access-Client-Secret", c.cfSecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect %s%s: %w", c.baseURL, path, err)
	}
	defer resp.Body.Close()

	// Bounded read with an explicit over-cap error. The previous silent 1 MiB
	// LimitReader truncated /api/mills/backlog once the ledger passed ~800
	// items (1.29 MiB on 2026-09-07) and surfaced as a baffling "unexpected
	// end of JSON input"; a truncated body must name the cap, not the parser.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, millsResponseBodyCap+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(respBody) > millsResponseBodyCap {
		return fmt.Errorf("read body: %s response exceeded the %d MiB cap; narrow the query (e.g. ?state=) or raise millsResponseBodyCap", path, millsResponseBodyCap>>20)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("operator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode %s: %w (body=%q)", path, err, truncateForError(respBody, 200))
	}
	return nil
}

// millsStatus mirrors the operator's /api/mills/status shape
// (cmd/loom-mills-operator/handlers_status.go). Fields an older operator does
// not populate arrive nil/zero and render as "—" in the human-readable view,
// so the CLI stays usable against any operator build.
type millsStatus struct {
	DBOK               bool   `json:"db_ok"`
	PolicyEnabled      bool   `json:"policy_enabled"`
	PolicyVersion      int    `json:"policy_version"`
	QueueDepth         *int   `json:"queue_depth,omitempty"`
	LastCouncilAt      string `json:"last_council_at,omitempty"`
	ActivePipelineRuns *int   `json:"active_pipeline_runs,omitempty"`
	Slice              string `json:"slice,omitempty"`

	BuildSHA         string                     `json:"build_sha,omitempty"`
	AutonomyReady    *bool                      `json:"autonomy_ready,omitempty"`
	AutonomyBlockers []string                   `json:"autonomy_blockers,omitempty"`
	Capabilities     []millsCapability          `json:"capabilities,omitempty"`
	Budget           map[string]millsBudgetTier `json:"budget,omitempty"`
	LastMergeAt      string                     `json:"last_merge_at,omitempty"`
	HealthGates      *millsHealthGates          `json:"health_gates,omitempty"`
	HealthGatesMode  string                     `json:"health_gates_mode,omitempty"`
	CouncilYield     *millsCouncilYield         `json:"council_yield,omitempty"`
}

// millsCapability is one row of the operator's capability matrix. Only id,
// status and mode are rendered; the rest stays reachable via --json.
type millsCapability struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	Mode                string `json:"mode"`
	RequiredForAutonomy bool   `json:"required_for_autonomy"`
}

// millsBudgetTier is a tier's rolling-24h spend against its policy caps
// (pkg/mills.WindowUsage). RunsCap 0 means "no run cap".
type millsBudgetTier struct {
	SpentUSD float64 `json:"spent_usd"`
	CapUSD   float64 `json:"cap_usd"`
	Runs     int     `json:"runs"`
	RunsCap  int     `json:"runs_cap"`
}

// millsHealthGates is the admission verdict projection
// (pkg/mills/gates.HealthGateReport), trimmed to what the one-line render uses.
type millsHealthGates struct {
	Allowed    bool     `json:"allowed"`
	FailClosed bool     `json:"fail_closed"`
	Status     string   `json:"status"`
	Reasons    []string `json:"reasons,omitempty"`
}

// millsCouncilYield mirrors the operator's council_yield block: finished
// council runs since one last produced a backlog delta, and their cost.
type millsCouncilYield struct {
	RunsSinceLastDelta    int     `json:"runs_since_last_delta"`
	CostSinceLastDeltaUSD float64 `json:"cost_since_last_delta_usd"`
	LastDeltaAt           string  `json:"last_delta_at,omitempty"`
	LastDeltaRunID        string  `json:"last_delta_run_id,omitempty"`
	SampleSize            int     `json:"sample_size"`
}

// millsStatusNow is the clock the human-readable render uses for relative
// ages ("4h ago"). Overridden in tests for deterministic output.
var millsStatusNow = time.Now

func newMillsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the mills's current state (operator health, policy, queue, last council)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := resolveMillsClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			emitJSON, _ := cmd.Flags().GetBool("json")
			if emitJSON {
				// Pass-through mode: fetch + reprint without re-marshaling so
				// extra fields the operator might add stay visible to scripts.
				var raw json.RawMessage
				if err := client.get(ctx, "/api/mills/status", &raw); err != nil {
					return wrapMillsErr(client, err)
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return err
			}

			var st millsStatus
			if err := client.get(ctx, "/api/mills/status", &st); err != nil {
				return wrapMillsErr(client, err)
			}
			return renderMillsStatus(cmd.OutOrStdout(), client.baseURL, st)
		},
	}
}

// renderMillsStatus prints the human-readable status block.
func renderMillsStatus(w io.Writer, base string, st millsStatus) error {
	enabled := "off"
	if st.PolicyEnabled {
		enabled = "on"
	}
	dbOK := "ok"
	if !st.DBOK {
		dbOK = "FAIL"
	}
	queue := "—"
	if st.QueueDepth != nil {
		queue = fmt.Sprintf("%d", *st.QueueDepth)
	}
	active := "—"
	if st.ActivePipelineRuns != nil {
		active = fmt.Sprintf("%d", *st.ActivePipelineRuns)
	}
	slice := st.Slice
	if slice == "" {
		slice = "(unknown)"
	}
	now := millsStatusNow()

	rows := [][2]string{
		{"policy", fmt.Sprintf("%s (v%d)", enabled, st.PolicyVersion)},
		{"store", dbOK},
		{"autonomy", renderMillsAutonomy(st)},
		{"health gates", renderMillsHealthGates(st)},
		{"capabilities", renderMillsCapabilities(st.Capabilities)},
		{"budget (24h)", renderMillsBudget(st.Budget)},
		{"queue depth", queue},
		{"active pipelines", active},
		{"last council run", renderMillsTimestamp(st.LastCouncilAt, now)},
		{"council yield", renderMillsCouncilYield(st.CouncilYield, now)},
		{"last merge", renderMillsTimestamp(st.LastMergeAt, now)},
		{"operator slice", slice},
	}
	if st.BuildSHA != "" {
		rows = append([][2]string{{"build", st.BuildSHA}}, rows...)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Loom Mills @ %s\n", base)
	for _, row := range rows {
		fmt.Fprintf(&b, "  %-17s %s\n", row[0]+":", row[1])
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderMillsAutonomy folds autonomy_ready + autonomy_blockers into one line.
// An operator that predates the capability report renders "—".
func renderMillsAutonomy(st millsStatus) string {
	if st.AutonomyReady == nil {
		return "—"
	}
	if *st.AutonomyReady {
		return "ready"
	}
	if len(st.AutonomyBlockers) == 0 {
		return "BLOCKED"
	}
	return "BLOCKED: " + strings.Join(st.AutonomyBlockers, "; ")
}

// renderMillsHealthGates renders the admission verdict with its mode, e.g.
// "pass (observe)". A blocking verdict carries its first reason so the
// operator does not have to open --json to learn why dispatch is held.
func renderMillsHealthGates(st millsStatus) string {
	hg := st.HealthGates
	if hg == nil {
		return "—"
	}
	status := hg.Status
	if status == "" {
		if hg.Allowed {
			status = "pass"
		} else {
			status = "block"
		}
	}
	out := status
	if st.HealthGatesMode != "" {
		out += " (" + st.HealthGatesMode + ")"
	}
	if !hg.Allowed && len(hg.Reasons) > 0 {
		out += " — " + hg.Reasons[0]
	}
	return out
}

// renderMillsCapabilities summarises the capability matrix as a green count
// and lists every non-green row inline ("10/11 green; hud_spawn=red(stub)")
// so a degraded operator is visible without --json.
func renderMillsCapabilities(caps []millsCapability) string {
	if len(caps) == 0 {
		return "—"
	}
	green := 0
	var degraded []string
	for _, c := range caps {
		if c.Status == "green" {
			green++
			continue
		}
		label := c.ID + "=" + c.Status
		if c.Mode != "" && c.Mode != "real" {
			label += "(" + c.Mode + ")"
		}
		if c.RequiredForAutonomy {
			label += "*"
		}
		degraded = append(degraded, label)
	}
	out := fmt.Sprintf("%d/%d green", green, len(caps))
	if len(degraded) > 0 {
		out += "; " + strings.Join(degraded, ", ")
	}
	return out
}

// renderMillsBudget renders each tier's rolling-24h spend against its caps,
// pipeline first, e.g. "pipeline $2.64/$75 (11/60 runs) · council $2.77/$50 (4 runs)".
func renderMillsBudget(budget map[string]millsBudgetTier) string {
	if len(budget) == 0 {
		return "—"
	}
	tiers := make([]string, 0, len(budget))
	for _, name := range []string{"pipeline", "council"} {
		if t, ok := budget[name]; ok {
			tiers = append(tiers, name+" "+renderMillsBudgetTier(t))
		}
	}
	// Any tier the CLI does not know by name still renders, sorted for a
	// stable line.
	var extra []string
	for name := range budget {
		if name != "pipeline" && name != "council" {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		tiers = append(tiers, name+" "+renderMillsBudgetTier(budget[name]))
	}
	return strings.Join(tiers, " · ")
}

func renderMillsBudgetTier(t millsBudgetTier) string {
	spend := "$" + fmtMillsUSD(t.SpentUSD)
	if t.CapUSD > 0 {
		spend += "/$" + fmtMillsUSD(t.CapUSD)
	}
	runs := fmt.Sprintf("%d", t.Runs)
	if t.RunsCap > 0 {
		runs += fmt.Sprintf("/%d", t.RunsCap)
	}
	return fmt.Sprintf("%s (%s runs)", spend, runs)
}

// fmtMillsUSD prints dollars with cents below $100 and whole dollars above,
// trimming a trailing ".00" so caps read as "$75" rather than "$75.00".
func fmtMillsUSD(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	s := fmt.Sprintf("%.2f", v)
	return strings.TrimSuffix(s, ".00")
}

// renderMillsCouncilYield turns the yield block into one sentence. Zero runs
// since the last delta reads as healthy; a dry spell names its cost, and a
// spell that exhausts the operator's sample says so instead of pretending
// the sample edge is where the spell began.
func renderMillsCouncilYield(y *millsCouncilYield, now time.Time) string {
	if y == nil {
		return "—"
	}
	if y.SampleSize == 0 {
		return "no finished council runs"
	}
	if y.RunsSinceLastDelta == 0 {
		return "last run produced backlog deltas"
	}
	spell := fmt.Sprintf("%d run%s without a backlog delta ($%s)",
		y.RunsSinceLastDelta, plural(y.RunsSinceLastDelta), fmtMillsUSD(y.CostSinceLastDeltaUSD))
	if y.LastDeltaAt == "" {
		return spell + fmt.Sprintf("; none in the last %d examined", y.SampleSize)
	}
	return spell + "; last delta " + renderMillsTimestamp(y.LastDeltaAt, now)
}

// renderMillsTimestamp keeps the operator's RFC3339 value (scripts and eyes
// both grep it) and appends a relative age when it parses.
func renderMillsTimestamp(ts string, now time.Time) string {
	if ts == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return ts + " (" + fmtMillsAge(now.Sub(t)) + ")"
}

// fmtMillsAge renders a duration as a coarse human age: "just now", "12m ago",
// "4h ago", "3d ago". Negative (clock skew) reads as "just now".
func fmtMillsAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// wrapMillsErr decorates connection errors with the friendly hint the CLI's
// users will most often need: how to point at the right operator URL.
func wrapMillsErr(c *millsClient, err error) error {
	if err == nil {
		return nil
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w\nhint: operator at %s is not responding within timeout — check the deployment is Ready or override --operator-url / LOOM_MILLS_OPERATOR_URL", err, c.baseURL)
	}
	if isConnRefused(err) {
		return fmt.Errorf("%w\nhint: nothing answering at %s — set LOOM_MILLS_OPERATOR_URL or use `kubectl port-forward -n loom-mills svc/loom-mills-operator 8090:8090`", err, c.baseURL)
	}
	return err
}

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "dial tcp")
}

func truncateForError(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
