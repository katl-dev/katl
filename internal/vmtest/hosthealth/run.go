package hosthealth

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Report is the host-health record of one run.
type Report struct {
	Start   Sample  `json:"start"`
	End     Sample  `json:"end"`
	Health  Health  `json:"health"`
	Verdict Verdict `json:"verdict"`
}

// Finish builds the report for a run from its starting sample, the serial
// logs under runDir and the go test log.
func Finish(start Sample, procRoot, runDir, goTestLogPath string, passed bool) (Report, error) {
	end, err := ReadSample(procRoot, time.Now().UTC())
	if err != nil {
		return Report{}, err
	}
	h := Between(start, end)
	_ = filepath.WalkDir(runDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, "serial.log") {
			if data, rerr := os.ReadFile(path); rerr == nil {
				h.ScanSerial(string(data))
			}
		}
		return nil
	})
	log, _ := os.ReadFile(goTestLogPath)
	return Report{Start: start, End: end, Health: h, Verdict: Classify(passed, string(log), h)}, nil
}

// ReadSampleFile loads a sample written by WriteJSON.
func ReadSampleFile(path string) (Sample, error) {
	var s Sample
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

// WriteJSON writes v as indented JSON.
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
