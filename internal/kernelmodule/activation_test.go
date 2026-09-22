package kernelmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequiredModuleHealth(t *testing.T) {
	for _, scenario := range []string{"load", "preloaded", "wrong provider", "false success", "wrong kernel", "wrong index", "missing identity", "preloaded missing identity"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			data := []byte("selected module")
			digest := sha256.Sum256(data)
			module := Module{
				Name:     "example",
				Path:     "usr/lib/modules/6.12.1/extra/example.ko",
				SHA256:   hex.EncodeToString(digest[:]),
				Required: true,
			}
			contracts := []Contract{{
				Target: Target{
					Release:       "6.12.1",
					RuntimeSHA256: strings.Repeat("a", 64),
				},
				Modules: []Module{module},
			}}
			writeModuleFile(t, root, module.Path, string(data))
			if scenario == "preloaded" || scenario == "wrong provider" || scenario == "preloaded missing identity" {
				value := "SELECTED"
				if scenario == "wrong provider" {
					value = "BASE_PROVIDER"
				}
				writeModuleFile(t, root, "sys/module/example/srcversion", value)
			}
			loads := 0
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				switch name {
				case "uname":
					if scenario == "wrong kernel" {
						return []byte("6.12.2"), nil
					}
					return []byte("6.12.1"), nil
				case "modinfo":
					if args[1] == "filename" {
						path := filepath.Join(root, module.Path)
						if scenario == "wrong index" {
							writeModuleFile(t, root, "base/example.ko", "base module")
							path = filepath.Join(root, "base/example.ko")
						}
						return []byte(path), nil
					}
					if strings.Contains(scenario, "missing identity") {
						return nil, nil
					}
					return []byte("SELECTED"), nil
				case "modprobe":
					loads++
					if strings.Join(args, " ") != "--ignore-install -- example" {
						t.Fatalf("unsafe loading command: %v", args)
					}
					if scenario != "false success" {
						writeModuleFile(t, root, "sys/module/example/srcversion", "SELECTED")
					}
					return nil, nil
				default:
					return nil, fmt.Errorf("unexpected command %s", name)
				}
			}

			err := requiredModules(context.Background(), root, contracts, true, run)
			if strings.Contains(scenario, "missing identity") {
				if err == nil || !strings.Contains(err.Error(), "source identity") || loads != 0 {
					t.Fatalf("unverifiable provider: err=%v loads=%d", err, loads)
				}
				writeModuleFile(t, root, "sys/module/example/srcversion", "UNVERIFIED")
				if err := requiredModules(context.Background(), root, contracts, false, run); err == nil {
					t.Fatal("health accepted a provider without a selected source identity")
				}
				return
			}
			if scenario != "load" && scenario != "preloaded" {
				if err == nil {
					t.Fatalf("accepted %s", scenario)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "preloaded" && loads != 0 {
				t.Fatal("attempted to replace a loaded driver")
			}
			if err := requiredModules(context.Background(), root, contracts, false, run); err != nil {
				t.Fatal(err)
			}
			if err := requiredModules(context.Background(), root, contracts, true, run); err != nil {
				t.Fatalf("repeat module activation: %v", err)
			}
			if err := os.RemoveAll(filepath.Join(root, "sys/module/example")); err != nil {
				t.Fatal(err)
			}
			if err := requiredModules(context.Background(), root, contracts, false, run); err == nil {
				t.Fatal("health accepted an unloaded required driver")
			}
		})
	}
}
