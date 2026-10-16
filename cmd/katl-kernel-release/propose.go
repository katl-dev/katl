package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/kernelrelease"
)

//go:embed update-commit.txt
var updateCommit string

func propose(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-kernel-release propose", flag.ContinueOnError)
	flags.SetOutput(stderr)
	candidates := flags.String("candidates", "_build/kernel-candidates", "captured standard and LTS inputs")
	repository := flags.String("repository", os.Getenv("GITHUB_REPOSITORY"), "GitHub repository")
	base := flags.String("base", os.Getenv("GITHUB_SHA"), "captured source commit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *repository == "" || *base == "" {
		return fmt.Errorf("propose requires --repository and --base")
	}
	var entries []map[string]string
	var identities []string
	expected := map[string]kernelrelease.Lock{}
	changed := false
	fedora := ""
	for _, flavour := range []string{"standard", "lts"} {
		path := filepath.Join(*candidates, flavour, "lock.json")
		lock, err := kernelrelease.Read(path)
		if err != nil {
			return err
		}
		if lock.Flavour != flavour || (fedora != "" && fedora != lock.FedoraRelease) {
			return fmt.Errorf("candidates must cover both flavours of one Fedora release")
		}
		fedora = lock.FedoraRelease
		identity, err := lock.Digest()
		if err != nil {
			return err
		}
		identities = append(identities, identity)
		destination := filepath.Join("kernels", "fedora-"+fedora, lock.Architecture, flavour+".json")
		expected[destination] = lock
		previous, err := kernelrelease.Read(destination)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			old, err := previous.Digest()
			if err != nil {
				return err
			}
			if old == identity {
				expected[destination] = previous
				continue
			}
		}
		changed = true
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, map[string]string{"path": destination, "mode": "100644", "type": "blob", "content": string(data)})
	}
	if !changed {
		return json.NewEncoder(stdout).Encode(map[string]any{"changed": false})
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(identities, "\n"))))
	branch := "automation/kernels-" + identity[:16]
	api := "repos/" + *repository
	var existing struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, err := ghAPI(ctx, httpGet, api+"/git/ref/heads/"+branch, nil, &existing)
	if err == nil {
		// Advance an existing proposal by merging main; never force-update or
		// recreate its history while reviewers may be looking at the branch.
		if _, err := ghAPI(ctx, "POST", api+"/merges", map[string]string{"base": branch, "head": *base}, nil); err != nil {
			return err
		}
		if _, err := ghAPI(ctx, httpGet, api+"/git/ref/heads/"+branch, nil, &existing); err != nil {
			return err
		}
		for path, wanted := range expected {
			var file struct {
				Content string `json:"content"`
			}
			if _, err := ghAPI(ctx, httpGet, api+"/contents/"+path+"?ref="+existing.Object.SHA, nil, &file); err != nil {
				return err
			}
			data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
			if err != nil {
				return err
			}
			var actual kernelrelease.Lock
			if err := json.Unmarshal(data, &actual); err != nil {
				return err
			}
			actualID, err := actual.Digest()
			if err != nil {
				return err
			}
			wantedID, err := wanted.Digest()
			if err != nil {
				return err
			}
			if actualID != wantedID || actual.Reference != wanted.Reference {
				return fmt.Errorf("existing proposal has different kernel inputs; preserve it for inspection")
			}
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"changed": true, "branch": branch, "commit": existing.Object.SHA})
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		return err
	}
	var parent struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := ghAPI(ctx, httpGet, api+"/git/commits/"+*base, nil, &parent); err != nil {
		return err
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if _, err := ghAPI(ctx, "POST", api+"/git/trees", map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}, &tree); err != nil {
		return err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if _, err := ghAPI(ctx, "POST", api+"/git/commits", map[string]any{"message": updateCommit, "tree": tree.SHA, "parents": []string{*base}}, &commit); err != nil {
		return err
	}
	if _, err := ghAPI(ctx, "POST", api+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA}, nil); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"changed": true, "branch": branch, "commit": commit.SHA})
}

const httpGet = "GET"

func ghAPI(ctx context.Context, method, endpoint string, input, output any) ([]byte, error) {
	args := []string{"api", "--method", method, endpoint}
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
		args = append(args, "--input", "-")
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(body)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("GitHub %s %s: %w: %s", method, endpoint, err, diagnostics.String())
	}
	if output != nil && len(bytes.TrimSpace(data)) != 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return nil, err
		}
	}
	return data, nil
}
