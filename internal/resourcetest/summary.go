package resourcetest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/gotestlog"
)

const SummaryKind = "ResourceTestSummary"

type SummaryStatus string

const (
	SummaryPassed SummaryStatus = "passed"
	SummaryFailed SummaryStatus = "failed"
)

type AggregateRequest struct {
	Manifest   Manifest
	GoTestJSON io.Reader
	ReadFile   func(string) ([]byte, error)
}

type Summary struct {
	APIVersion     string            `json:"apiVersion"`
	Kind           string            `json:"kind"`
	RunID          string            `json:"runID"`
	Status         SummaryStatus     `json:"status"`
	Counts         map[Status]int    `json:"counts"`
	Scenarios      []ScenarioSummary `json:"scenarios"`
	GoTestFailures []GoTestFailure   `json:"goTestFailures,omitempty"`
	Created        time.Time         `json:"created,omitempty"`
}

type ScenarioSummary struct {
	Name                 string   `json:"name"`
	Suite                string   `json:"suite"`
	GoPackage            string   `json:"goPackage,omitempty"`
	GoTest               string   `json:"goTest,omitempty"`
	Status               Status   `json:"status"`
	ResultPath           string   `json:"resultPath,omitempty"`
	RunDir               string   `json:"runDir,omitempty"`
	FailureSummary       string   `json:"failureSummary,omitempty"`
	FixtureRefs          []string `json:"fixtureRefs,omitempty"`
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`
}

type GoTestFailure struct {
	Package string `json:"package"`
	Test    string `json:"test,omitempty"`
	Action  string `json:"action"`
	Output  string `json:"output,omitempty"`
}

type MissingPrerequisite struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type scenarioArtifact struct {
	ScenarioName   string                `json:"scenarioName"`
	Status         string                `json:"status"`
	RunID          string                `json:"runId"`
	RunDir         string                `json:"runDir"`
	FailureSummary string                `json:"failureSummary"`
	Missing        []MissingPrerequisite `json:"missing"`
}

func Aggregate(request AggregateRequest) (Summary, error) {
	manifest := request.Manifest
	if err := ValidateManifest(manifest); err != nil {
		return Summary{}, err
	}
	readFile := request.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	events, failures, err := parseGoTestJSON(request.GoTestJSON)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{
		APIVersion:     APIVersion,
		Kind:           SummaryKind,
		RunID:          manifest.RunID,
		Status:         SummaryPassed,
		Counts:         map[Status]int{},
		GoTestFailures: failures,
		Created:        time.Now().UTC(),
	}
	if len(failures) > 0 {
		summary.Status = SummaryFailed
	}
	for _, scenario := range manifest.Scenarios {
		item := ScenarioSummary{
			Name:                 scenario.Name,
			Suite:                scenario.Suite,
			GoPackage:            scenario.GoPackage,
			GoTest:               scenario.GoTest,
			Status:               scenario.Status,
			ResultPath:           scenario.ResultPath,
			RunDir:               scenario.RunDir,
			FailureSummary:       scenario.FailureSummary,
			FixtureRefs:          append([]string(nil), scenario.FixtureRefs...),
			RequiredCapabilities: append([]string(nil), scenario.RequiredCapabilities...),
		}
		if scenario.Status == StatusDisabled {
			recordScenario(&summary, item)
			continue
		}
		item = aggregateScenario(item, manifest.RunID, scenario, events, readFile)
		recordScenario(&summary, item)
	}
	return summary, nil
}

func SummaryExitCode(summary Summary) int {
	if summary.Status == SummaryPassed {
		return 0
	}
	return 1
}

func EncodeSummary(w io.Writer, summary Summary) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func aggregateScenario(item ScenarioSummary, runID string, scenario Scenario, events map[string]gotestlog.Event, readFile func(string) ([]byte, error)) ScenarioSummary {
	if scenario.ResultPath == "" {
		item.Status = StatusSetupFailed
		item.FailureSummary = firstNonEmpty(item.FailureSummary, "resource scenario is missing resultPath")
		return item
	}
	data, err := readFile(scenario.ResultPath)
	if err != nil {
		item.Status = StatusSetupFailed
		item.FailureSummary = "scenario result missing: " + err.Error()
		return item
	}
	var artifact scenarioArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		item.Status = StatusSetupFailed
		item.FailureSummary = "scenario result is invalid: " + err.Error()
		return item
	}
	if artifact.RunDir != "" {
		item.RunDir = artifact.RunDir
	}
	if artifact.ScenarioName != "" && artifact.ScenarioName != scenario.Name {
		item.Status = StatusSetupFailed
		item.FailureSummary = fmt.Sprintf("scenario result is stale: expected %q, got %q", scenario.Name, artifact.ScenarioName)
		return item
	}
	if artifact.RunID != "" && artifact.RunID != runID {
		item.Status = StatusSetupFailed
		item.FailureSummary = fmt.Sprintf("scenario result is stale: expected run %q, got %q", runID, artifact.RunID)
		return item
	}
	if artifact.FailureSummary != "" {
		item.FailureSummary = artifact.FailureSummary
	}
	item.Status = classifyArtifact(artifact)
	if item.Status == StatusSetupFailed && item.FailureSummary == "" {
		item.FailureSummary = "scenario did not complete successfully"
	}
	if item.GoPackage != "" || item.GoTest != "" {
		event, ok := events[goTestKey(item.GoPackage, item.GoTest)]
		if !ok {
			item.Status = StatusSetupFailed
			item.FailureSummary = firstNonEmpty(item.FailureSummary, "go test result event is missing")
		} else if event.Action == "skip" && item.Status != StatusHostSkipped {
			item.Status = StatusSetupFailed
			item.FailureSummary = firstNonEmpty(item.FailureSummary, "go test skipped without declared host capability gap")
		} else if event.Action == "fail" && item.Status == StatusPassed {
			item.Status = StatusFailed
			item.FailureSummary = firstNonEmpty(item.FailureSummary, "go test reported failure")
		}
	}
	return item
}

func classifyArtifact(artifact scenarioArtifact) Status {
	switch artifact.Status {
	case "passed", "pass":
		return StatusPassed
	case "failed", "fail":
		return StatusFailed
	case "skipped", "skip":
		if len(artifact.Missing) > 0 {
			return StatusHostSkipped
		}
		return StatusSetupFailed
	case "planned", "":
		return StatusSetupFailed
	default:
		return StatusSetupFailed
	}
}

func recordScenario(summary *Summary, item ScenarioSummary) {
	summary.Scenarios = append(summary.Scenarios, item)
	summary.Counts[item.Status]++
	switch item.Status {
	case StatusPassed, StatusHostSkipped, StatusDisabled:
	default:
		summary.Status = SummaryFailed
	}
}

func parseGoTestJSON(r io.Reader) (map[string]gotestlog.Event, []GoTestFailure, error) {
	events := map[string]gotestlog.Event{}
	var failures []GoTestFailure
	if r == nil {
		return events, failures, nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event gotestlog.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, nil, fmt.Errorf("decode go test JSON: %w", err)
		}
		switch {
		case event.IsResult():
			key := goTestKey(event.Package, event.Test)
			last := events[key]
			if last.Output != "" && event.Output == "" {
				event.Output = last.Output
			}
			events[key] = event
			if event.Action == "fail" {
				failures = append(failures, GoTestFailure{Package: event.Package, Test: event.Test, Action: event.Action})
			}
		case event.Action == "output":
			key := goTestKey(event.Package, event.Test)
			last := events[key]
			last.Package = event.Package
			last.Test = event.Test
			last.Output += event.Output
			events[key] = last
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	for i, failure := range failures {
		event := events[goTestKey(failure.Package, failure.Test)]
		failures[i].Output = strings.TrimSpace(event.Output)
	}
	return events, failures, nil
}

func goTestKey(pkg, test string) string {
	return pkg + "\x00" + test
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func ValidateSummary(summary Summary) error {
	if summary.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q", APIVersion)
	}
	if summary.Kind != SummaryKind {
		return fmt.Errorf("kind must be %q", SummaryKind)
	}
	if strings.TrimSpace(summary.RunID) == "" {
		return errors.New("runID is required")
	}
	switch summary.Status {
	case SummaryPassed, SummaryFailed:
	default:
		return fmt.Errorf("summary status %q is unsupported", summary.Status)
	}
	for i, scenario := range summary.Scenarios {
		if strings.TrimSpace(scenario.Name) == "" {
			return fmt.Errorf("scenarios[%d]: name is required", i)
		}
		if !validStatus(scenario.Status) {
			return fmt.Errorf("scenarios[%d]: status %q is unsupported", i, scenario.Status)
		}
	}
	return nil
}
