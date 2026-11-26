package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

func driverGoMod(data []byte) ([]byte, error) {
	cmd := exec.Command("go", "mod", "edit", "-json", "/dev/stdin")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read Go module directives: %w", err)
	}

	var directives map[string]json.RawMessage
	if err := json.Unmarshal(output, &directives); err != nil {
		return nil, err
	}
	// Moving a requirement between direct and indirect blocks can change both
	// its annotation and its position without changing the selected version.
	var requirements []struct{ Path, Version string }
	if err := json.Unmarshal(directives["Require"], &requirements); err != nil {
		return nil, err
	}
	slices.SortFunc(requirements, func(a, b struct{ Path, Version string }) int {
		return strings.Compare(a.Path, b.Path)
	})
	directives["Require"], err = json.Marshal(requirements)
	if err != nil {
		return nil, err
	}
	return json.Marshal(directives)
}
