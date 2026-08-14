package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/crb2nu/loom/internal/fleetgate"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-reliability-gate:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: fleet-reliability-gate <test-plan|verify-tests|compare-benchmarks|write-metadata> [flags]")
	}
	switch args[0] {
	case "test-plan":
		return writeTestPlan(args[1:], stdout, stderr)
	case "verify-tests":
		return verifyTests(args[1:], stdout, stderr)
	case "compare-benchmarks":
		return compareBenchmarks(args[1:], stdout, stderr)
	case "write-metadata":
		return writeMetadata(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func writeTestPlan(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("test-plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "suite manifest path")
	group := flags.String("group", "", "test group name")
	field := flags.String("field", "", "field to print: regex or packages")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := fleetgate.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	plan, err := manifest.Plan(*group)
	if err != nil {
		return err
	}
	switch *field {
	case "regex":
		_, err = fmt.Fprintln(stdout, plan.Regex)
	case "packages":
		for _, pkg := range plan.Packages {
			if _, err = fmt.Fprintln(stdout, pkg); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown test-plan field %q (want regex or packages)", *field)
	}
	return err
}

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func writeMetadata(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("write-metadata", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "suite manifest path")
	output := flags.String("output", "", "JSON report path")
	buildSHA := flags.String("build-sha", "", "candidate git SHA")
	baseSHA := flags.String("base-sha", "", "merge-base git SHA")
	dirty := flags.Bool("dirty", false, "candidate worktree has tracked changes")
	configDigest := flags.String("config-digest", "", "CI/config SHA-256")
	schemaDigest := flags.String("schema-digest", "", "contract/schema SHA-256")
	command := flags.String("command", "", "gate command")
	goVersion := flags.String("go-version", "", "go version output")
	var scenarioFlags repeatedFlag
	flags.Var(&scenarioFlags, "scenario", "scenario count as name=count (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := fleetgate.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	counts := make(map[string]int, len(scenarioFlags))
	for _, value := range scenarioFlags {
		name, rawCount, found := strings.Cut(value, "=")
		if !found {
			return fmt.Errorf("invalid --scenario %q", value)
		}
		count, err := strconv.Atoi(rawCount)
		if err != nil {
			return fmt.Errorf("invalid --scenario %q: %w", value, err)
		}
		counts[name] = count
	}
	metadata, err := fleetgate.NewEvidenceMetadata(manifest, *buildSHA, *baseSHA, *dirty, *configDigest, *schemaDigest, *command, *goVersion, counts)
	if err != nil {
		return err
	}
	return writeReport(*output, metadata, stdout)
}

func verifyTests(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-tests", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "suite manifest path")
	group := flags.String("group", "", "test group name")
	input := flags.String("input", "", "go test -json input")
	output := flags.String("output", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := fleetgate.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	in, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer in.Close()
	report, verifyErr := fleetgate.VerifyTestEvents(manifest, *group, in)
	if err := writeReport(*output, report, stdout); err != nil {
		return err
	}
	return verifyErr
}

func compareBenchmarks(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("compare-benchmarks", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "suite manifest path")
	baselinePath := flags.String("baseline", "", "baseline benchmark output")
	candidatePath := flags.String("candidate", "", "candidate benchmark output")
	output := flags.String("output", "", "JSON report path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := fleetgate.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	baseline, err := os.Open(*baselinePath)
	if err != nil {
		return err
	}
	defer baseline.Close()
	candidate, err := os.Open(*candidatePath)
	if err != nil {
		return err
	}
	defer candidate.Close()
	report, compareErr := fleetgate.CompareBenchmarks(manifest, baseline, candidate)
	if err := writeReport(*output, report, stdout); err != nil {
		return err
	}
	return compareErr
}

func writeReport(path string, value any, stdout io.Writer) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" || path == "-" {
		_, err = stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
