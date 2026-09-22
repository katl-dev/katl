package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
)

type hostExtensionPlan struct {
	document  []byte
	manifest  manifest.Manifest
	desired   []manifest.SystemExtension
	materials []configapply.SystemExtensionPayload
	replace   []string
	sysexts   []generation.ExtensionRef
	confexts  []generation.ExtensionRef
}

func planHostExtensions(ctx context.Context, current manifest.Manifest, payload katlosimage.Payload, candidate string, fetch func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error)) (hostExtensionPlan, error) {
	if fetch == nil {
		fetch = payload.ResolveReleaseExtension
	}
	plan := hostExtensionPlan{manifest: current}
	plan.manifest.Node.SystemExtensions = slices.Clone(current.Node.SystemExtensions)
	root := generation.RootSelection{
		RuntimeVersion:        payload.Index.Version,
		Architecture:          payload.Index.Architecture,
		Flavour:               payload.Index.Flavour,
		RuntimeInterface:      payload.Index.RuntimeInterface,
		RuntimeArtifactSHA256: payload.Runtime.SHA256,
	}
	for i, selected := range current.Node.SystemExtensions {
		if selected.Release == "" {
			// An explicit selection preserves its resolved artifact, even when its
			// original tag has moved. Generation validation checks compatibility.
			continue
		}
		if payload.Index.ExtensionRelease == nil {
			return hostExtensionPlan{}, fmt.Errorf("target release does not provide extension %q; choose a release that includes it", selected.Release)
		}
		desired, resolved, err := systemextensionbundle.ResolveSelection(ctx, payload.Index.ExtensionRelease.Target, payload.Index.ExtensionRelease, selected, fetch)
		if err != nil {
			return hostExtensionPlan{}, fmt.Errorf("resolve upgrade extension %q: %w", selected.Repository(), err)
		}
		plan.manifest.Node.SystemExtensions[i] = desired
		plan.desired = append(plan.desired, desired)
		for _, previous := range selected.Payloads {
			plan.replace = append(plan.replace, selected.PayloadID(previous.Name))
		}
		for _, content := range resolved.Payloads {
			plan.materials = append(plan.materials, configapply.SystemExtensionPayload{
				Ref: manifest.SystemExtensionPayloadRef{
					Name:      content.Descriptor.FileName,
					Role:      content.Descriptor.Role,
					MediaType: content.Descriptor.MediaType,
					Digest:    content.Descriptor.Digest,
					SizeBytes: content.Descriptor.SizeBytes,
				},
				Data: content.Data,
			})
		}
	}
	var err error
	plan.sysexts, plan.confexts, err = configapply.PlanSystemExtensions(candidate, root, payload.Index.ExtensionRelease, plan.desired, plan.materials)
	if err != nil {
		return hostExtensionPlan{}, err
	}
	// KatlosImage describes installation input. The generation spec owns the
	// running runtime identity; resolved extension selections above follow it.
	return plan, nil
}
