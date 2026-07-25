package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func nodeSystemExtensionStatus(ctx context.Context, root, currentGeneration, desiredGeneration string, runner ToolRunner) ([]*agentapi.SystemExtensionStatus, error) {
	currentManifest, currentErr := generationManifest(root, currentGeneration)
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return nil, currentErr
	}
	desiredManifest, desiredErr := generationManifest(root, desiredGeneration)
	if desiredErr != nil {
		if !errors.Is(desiredErr, os.ErrNotExist) {
			return nil, desiredErr
		}
		desiredManifest = currentManifest
	}
	currentSpec, _, currentSpecErr := generation.ReadGeneration(root, currentGeneration)
	if currentSpecErr != nil && currentGeneration != "" {
		return nil, currentSpecErr
	}
	desiredSpec, _, desiredSpecErr := generation.ReadGeneration(root, desiredGeneration)
	if desiredSpecErr != nil {
		if desiredGeneration != currentGeneration && !errors.Is(desiredSpecErr, os.ErrNotExist) {
			return nil, desiredSpecErr
		}
		desiredSpec = currentSpec
	}
	current := extensionsByName(currentManifest.Node.SystemExtensions)
	desired := extensionsByName(desiredManifest.Node.SystemExtensions)
	names := make([]string, 0, len(current)+len(desired))
	seen := make(map[string]struct{}, len(current)+len(desired))
	for name := range current {
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for name := range desired {
		if _, ok := seen[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]*agentapi.SystemExtensionStatus, 0, len(names))
	for _, name := range names {
		want, wanted := desired[name]
		observed, observedNow := current[name]
		status := &agentapi.SystemExtensionStatus{
			Name:                 name,
			DesiredState:         manifest.SystemExtensionAbsent,
			DesiredGenerationId:  desiredGeneration,
			ObservedGenerationId: currentGeneration,
			RebootRequired:       desiredGeneration != currentGeneration,
			StagingState:         "not-selected",
			ActivationState:      "inactive",
			Compatibility:        "unknown",
		}
		if wanted {
			status.DesiredState = manifest.SystemExtensionPresent
			status.SubmittedReference = want.Bundle
			status.OciManifestDigest = want.OCIManifestDigest
			status.BundleManifestDigest = want.BundleManifestDigest
			status.ArtifactVersion = want.ArtifactVersion
			status.PayloadVersion = want.PayloadVersion
			status.Architecture = want.Architecture
			status.SupportedRuntimeInterfaces = append([]string(nil), want.SupportedRuntimeInterfaces...)
			status.Compatibility = extensionCompatibility(want, desiredSpec.Root)
			status.StagingState = extensionStagingState(root, desiredSpec, want)
			status.Files = extensionFileStatus(want)
			status.Payloads = extensionPayloadStatus(root, currentSpec, desiredSpec, want)
		}
		if observedNow {
			status.ActivationState = extensionActivationState(root, currentSpec, observed)
		}
		unitSource := want
		if !wanted {
			unitSource = observed
		}
		for _, unit := range unitSource.Units {
			status.Units = append(status.Units, systemExtensionUnitStatus(ctx, runner, unit))
		}
		out = append(out, status)
	}
	return out, nil
}

func generationManifest(root, generationID string) (manifest.Manifest, error) {
	if strings.TrimSpace(generationID) == "" {
		return manifest.Manifest{}, os.ErrNotExist
	}
	path := filepath.Join(filepath.Clean(root), strings.TrimPrefix(generation.GenerationRecordsDir, "/"), generationID, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest.Manifest{}, err
	}
	value, _, err := manifest.DecodeWithOptions(bytes.NewReader(data), manifest.DecodeOptions{AllowMissingKatlosImage: true})
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("decode generation %s manifest: %w", generationID, err)
	}
	return value, nil
}

func extensionsByName(extensions []manifest.SystemExtension) map[string]manifest.SystemExtension {
	out := make(map[string]manifest.SystemExtension, len(extensions))
	for _, extension := range extensions {
		out[extension.Name] = extension
	}
	return out
}

func extensionCompatibility(extension manifest.SystemExtension, root generation.RootSelection) string {
	if extension.Architecture != root.Architecture {
		return "incompatible"
	}
	for _, runtime := range extension.SupportedRuntimeInterfaces {
		if runtime == root.RuntimeInterface {
			return "compatible"
		}
	}
	return "incompatible"
}

func extensionStagingState(root string, spec generation.GenerationSpec, extension manifest.SystemExtension) string {
	for _, payload := range extension.Payloads {
		ref, ok := extensionPayloadRef(spec, payload)
		if !ok {
			return "missing"
		}
		path := filepath.Join(filepath.Clean(root), strings.TrimPrefix(ref.Path, "/"))
		data, err := os.ReadFile(path)
		if err != nil || digestStatusBytes(data) != payload.Digest {
			return "missing"
		}
	}
	return "staged"
}

func extensionActivationState(root string, spec generation.GenerationSpec, extension manifest.SystemExtension) string {
	if len(extension.Payloads) == 0 {
		return "inactive"
	}
	for _, payload := range extension.Payloads {
		ref, ok := extensionPayloadRef(spec, payload)
		if !ok {
			return "inactive"
		}
		link := filepath.Join(filepath.Clean(root), strings.TrimPrefix(ref.ActivationPath, "/"))
		target, err := os.Readlink(link)
		if err != nil || filepath.ToSlash(target) != ref.Path {
			return "inactive"
		}
	}
	return "active"
}

func extensionPayloadRef(spec generation.GenerationSpec, payload manifest.SystemExtensionPayloadRef) (generation.ExtensionRef, bool) {
	refs := spec.Sysexts
	if payload.Role == "systemd-confext" {
		refs = spec.BundledConfexts
	}
	root := generation.DefaultExtensionsActivationDir
	if payload.Role == "systemd-confext" {
		root = generation.DefaultConfextsActivationDir
	}
	for _, ref := range refs {
		if ref.ActivationPath == root+"/"+payload.Name && "sha256:"+ref.SHA256 == payload.Digest {
			return ref, true
		}
	}
	return generation.ExtensionRef{}, false
}

func extensionPayloadStatus(root string, current, desired generation.GenerationSpec, extension manifest.SystemExtension) []*agentapi.SystemExtensionPayloadStatus {
	out := make([]*agentapi.SystemExtensionPayloadStatus, 0, len(extension.Payloads))
	for _, payload := range extension.Payloads {
		desiredRef, selected := extensionPayloadRef(desired, payload)
		currentRef, activeSelected := extensionPayloadRef(current, payload)
		active := false
		if activeSelected {
			link := filepath.Join(filepath.Clean(root), strings.TrimPrefix(currentRef.ActivationPath, "/"))
			target, err := os.Readlink(link)
			active = err == nil && filepath.ToSlash(target) == currentRef.Path
		}
		activationPath := ""
		if selected {
			activationPath = desiredRef.ActivationPath
		}
		out = append(out, &agentapi.SystemExtensionPayloadStatus{
			Name: payload.Name, Role: payload.Role, Digest: payload.Digest, SizeBytes: payload.SizeBytes,
			ActivationPath: activationPath, Selected: selected, Active: active,
		})
	}
	return out
}

func extensionFileStatus(extension manifest.SystemExtension) []*agentapi.SystemExtensionFileStatus {
	var out []*agentapi.SystemExtensionFileStatus
	for _, file := range extension.Configuration.Files {
		if file.Content == nil {
			continue
		}
		mode := file.Mode
		if mode == 0 {
			mode = 0o644
		}
		sum := sha256.Sum256([]byte(*file.Content))
		out = append(out, &agentapi.SystemExtensionFileStatus{
			Path: file.Path, Sha256: hex.EncodeToString(sum[:]), Mode: mode,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func systemExtensionUnitStatus(ctx context.Context, runner ToolRunner, unit manifest.SystemExtensionUnit) *agentapi.SystemExtensionUnitStatus {
	out := &agentapi.SystemExtensionUnitStatus{
		Name: unit.Name, Enable: unit.Enable, RequiredForBootHealth: unit.RequiredForBootHealth,
		LoadState: "unknown", ActiveState: "unknown", SubState: "unknown", Result: "unknown",
	}
	if runner == nil {
		return out
	}
	result := runner(ctx, []string{"systemctl", "show", unit.Name, "--no-pager",
		"--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=Result", "--property=StateChangeTimestamp"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		out.FailureDiagnostic = boundedToolDiagnostic(result)
		return out
	}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			out.LoadState = value
		case "ActiveState":
			out.ActiveState = value
		case "SubState":
			out.SubState = value
		case "Result":
			out.Result = value
		case "StateChangeTimestamp":
			out.StateChangeTimestamp = value
		}
	}
	if out.ActiveState == "failed" || (out.Result != "" && out.Result != "success") {
		journal := runner(ctx, []string{"journalctl", "--unit", unit.Name, "--lines=20", "--no-pager", "--output=short-monotonic"}, nil)
		out.FailureDiagnostic = boundedToolDiagnostic(journal)
	}
	return out
}

func boundedDiagnostic(data []byte) string {
	const limit = 4096
	value := strings.TrimSpace(string(data))
	if len(value) > limit {
		value = value[len(value)-limit:]
	}
	return value
}

func boundedToolDiagnostic(result ToolResult) string {
	data := append(append([]byte(nil), result.Stdout...), result.Stderr...)
	return boundedDiagnostic(data)
}

func digestStatusBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
