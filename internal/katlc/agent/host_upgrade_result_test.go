package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

func TestPreparedUpgradeResultAcceptsOpaqueCandidate(t *testing.T) {
	root := t.TempDir()
	writeResetGenerationZero(t, root)
	spec, _, err := generation.ReadGeneration(root, "0")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "var/lib/katl/generations/0/spec.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["futureRecordField"] = json.RawMessage(`{"future":true}`)
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	handoff, payload := resultTestInputs(spec)
	resultPath := preparedUpgradeResultPath(root, "0")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePreparedUpgradeResult(root, handoff, spec); err != nil {
		t.Fatal(err)
	}
	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultData, &result); err != nil {
		t.Fatal(err)
	}
	result["futureResultField"] = json.RawMessage(`{"future":true}`)
	resultData, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, resultData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedUpgradeResult(root, handoff, payload); err != nil {
		t.Fatalf("future target output was rejected: %v", err)
	}
}

func TestPreparedUpgradeResultRejectsChangedBootAndCandidate(t *testing.T) {
	root := t.TempDir()
	writeResetGenerationZero(t, root)
	spec, _, err := generation.ReadGeneration(root, "0")
	if err != nil {
		t.Fatal(err)
	}
	handoff, payload := resultTestInputs(spec)
	resultPath := preparedUpgradeResultPath(root, "0")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePreparedUpgradeResult(root, handoff, spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var result preparedUpgradeResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	result.RootSlot = "root-b"
	changed, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedUpgradeResult(root, handoff, payload); err == nil || !strings.Contains(err.Error(), "boot contract") {
		t.Fatalf("changed boot selection error = %v", err)
	}
	result.RootSlot = handoff.RootSlot
	result.Version++
	changed, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedUpgradeResult(root, handoff, payload); err == nil || !strings.Contains(err.Error(), "ABI version") {
		t.Fatalf("unsupported ABI version error = %v", err)
	}
	if err := os.WriteFile(resultPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var/lib/katl/generations/0/extra"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedUpgradeResult(root, handoff, payload); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("changed candidate error = %v", err)
	}
}

func resultTestInputs(spec generation.GenerationSpec) (generation.UpgradeHandoff, katlosimage.Payload) {
	handoff := generation.UpgradeHandoff{
		OperationID: "test-operation", SourceGenerationID: "source", CandidateGenerationID: spec.GenerationID,
		ImageSHA256: strings.Repeat("b", 64), RootSlot: spec.Root.Slot, RootPartitionUUID: spec.Root.PartitionUUID,
		UKIPath: spec.Boot.UKIPath, LoaderEntryPath: spec.Boot.LoaderEntryPath,
	}
	payload := katlosimage.Payload{Index: katlosimage.Index{Version: spec.RuntimeVersion}, Runtime: katlosimage.Component{SHA256: spec.Root.RuntimeArtifactSHA256}}
	return handoff, payload
}
