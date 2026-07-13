package operatorconsole

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func WriteHandoff(path, url string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("handoff projection path is required")
	}
	record := Handoff{
		URL:       strings.TrimRight(strings.TrimSpace(url), "/") + "/v1/config-bundle",
		UpdatedAt: time.Now().UTC(),
	}
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("handoff URL is required")
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal handoff projection: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create handoff projection directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".handoff-*")
	if err != nil {
		return fmt.Errorf("create handoff projection: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect handoff projection: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write handoff projection: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close handoff projection: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish handoff projection: %w", err)
	}
	return nil
}

func ReadHandoff(path string) (Handoff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Handoff{}, err
	}
	var record Handoff
	if err := json.Unmarshal(data, &record); err != nil {
		return Handoff{}, fmt.Errorf("decode handoff projection: %w", err)
	}
	return record, nil
}
