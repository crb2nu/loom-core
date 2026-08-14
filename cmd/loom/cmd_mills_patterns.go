// cmd_mills_patterns.go implements the Pattern Loom human front door (Slice B1):
//
//	loom mills patterns [--status approved]   — list the pattern catalog.
//	loom mills stamp --pattern <id> --materials <json|@file> [--project <p>]
//	                                          — stamp a pattern into a Plan.
//
// Unlike the other `loom mills` subcommands (which are HTTP clients to the
// in-cluster operator), patterns live in the agent-context store. These commands
// therefore talk to the local agent-context daemon over the loom socket via the
// shared withAgentBridge helper — the same path cmd_agent_context.go uses.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// newMillsPatternsCmd returns `loom mills patterns` — list the pattern catalog.
func newMillsPatternsCmd() *cobra.Command {
	var status string

	cmd := &cobra.Command{
		Use:   "patterns",
		Short: "List the Pattern Loom catalog (vetted product archetypes)",
		Long: `List patterns from the shared agent-context catalog.

A Pattern is a vetted product archetype that, given Materials, is stamped into a
Plan that Mills executes. Filter by approval status with --status
(candidate|approved|deprecated).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := withAgentBridge(cmd, func(b *bridge.AgentBridge) (json.RawMessage, error) {
				patterns, err := b.PatternList(strings.TrimSpace(status))
				if err != nil {
					return nil, err
				}
				return json.Marshal(patterns)
			})
			if err != nil {
				return err
			}

			emitJSON, _ := cmd.Flags().GetBool("json")
			if emitJSON {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), string(result))
				return err
			}

			var patterns []bridge.PatternInfo
			if err := json.Unmarshal(result, &patterns); err != nil {
				return fmt.Errorf("decode patterns: %w", err)
			}
			if len(patterns) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no patterns)")
				return nil
			}
			renderPatternsTable(cmd.OutOrStdout(), patterns)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter by approval status (candidate|approved|deprecated)")
	return cmd
}

// renderPatternsTable prints one row per pattern using a tabwriter so columns
// align even when ids/makes vary widely in length.
func renderPatternsTable(w io.Writer, patterns []bridge.PatternInfo) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATTERN ID\tMAKES\tVERSION\tSTATUS\tMATERIALS")
	for _, p := range patterns {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			p.ID,
			truncate(valueOrDash(p.Makes), 40),
			valueOrDash(p.Version),
			valueOrDash(p.Status),
			len(p.MaterialsSchema),
		)
	}
	_ = tw.Flush()
}

// newMillsStampCmd returns `loom mills stamp` — stamp a pattern into a Plan.
func newMillsStampCmd() *cobra.Command {
	var (
		patternID string
		materials string
		project   string
	)

	cmd := &cobra.Command{
		Use:   "stamp",
		Short: "Stamp a Pattern with Materials, expanding it into an executable Plan",
		Long: `Stamp a Pattern: validate Materials against the pattern's schema, then expand
its slice_template into a concrete Plan in the shared store (executable by Mills).

--materials accepts an inline JSON object, or @path / a readable file path whose
contents are a JSON object.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			patternID = strings.TrimSpace(patternID)
			if patternID == "" {
				return fmt.Errorf("--pattern is required")
			}
			mats, err := parseMaterialsArg(materials)
			if err != nil {
				return err
			}

			result, err := withAgentBridge(cmd, func(b *bridge.AgentBridge) (json.RawMessage, error) {
				res, err := b.PatternStamp(patternID, mats, strings.TrimSpace(project))
				if err != nil {
					return nil, err
				}
				return json.Marshal(res)
			})
			if err != nil {
				return err
			}

			emitJSON, _ := cmd.Flags().GetBool("json")
			if emitJSON {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), string(result))
				return err
			}

			var res bridge.PatternStampResult
			if err := json.Unmarshal(result, &res); err != nil {
				return fmt.Errorf("decode stamp result: %w", err)
			}
			return renderStampResult(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&patternID, "pattern", "", "Pattern id to stamp (e.g. pattern-go-rest-service) [required]")
	cmd.Flags().StringVar(&materials, "materials", "", "Materials as inline JSON, @file, or a path to a JSON file [required]")
	cmd.Flags().StringVar(&project, "project", "", "Canonical project id for the stamped plan")
	return cmd
}

// parseMaterialsArg resolves the --materials value into a JSON object. It
// accepts an inline JSON object, an @path reference, or a bare path to a
// readable file. The decoded value must be a JSON object (the materials map).
func parseMaterialsArg(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("--materials is required (inline JSON, @file, or a path to a JSON file)")
	}

	data := []byte(raw)
	// @file or a bare path that exists on disk: read the file contents.
	if strings.HasPrefix(raw, "@") {
		path := strings.TrimSpace(raw[1:])
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read materials file %q: %w", path, err)
		}
		data = b
	} else if !strings.HasPrefix(raw, "{") {
		// Not inline JSON — treat as a file path if it exists.
		if b, err := os.ReadFile(raw); err == nil {
			data = b
		}
	}

	var mats map[string]any
	if err := json.Unmarshal(data, &mats); err != nil {
		return nil, fmt.Errorf("materials must be a JSON object: %w", err)
	}
	if len(mats) == 0 {
		return nil, fmt.Errorf("materials object is empty")
	}
	return mats, nil
}

// renderStampResult prints the human-readable summary of a stamp: the new plan
// id, slice count, and the required tools the caller must provide.
func renderStampResult(w io.Writer, res bridge.PatternStampResult) error {
	tools := "—"
	if len(res.ToolsRequired) > 0 {
		tools = strings.Join(res.ToolsRequired, ", ")
	}
	_, err := fmt.Fprintf(w,
		"Stamped %s → plan %s\n  slices:         %d\n  tools required: %s\n",
		valueOrDash(res.PatternID), valueOrDash(res.PlanID), res.SliceCount, tools,
	)
	return err
}
