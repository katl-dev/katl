package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
)

type systemExtensionStatusOptions struct {
	target  managementTargetOptions
	timeout time.Duration
	output  string
}

type systemExtensionStatusReport struct {
	Node       string                            `json:"node"`
	Extensions []*agentapi.SystemExtensionStatus `json:"extensions"`
}

func newSystemExtensionCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "system-extension", Short: "Inspect user-owned system extensions"}
	cmd.AddCommand(newSystemExtensionStatusCommand(ctx, stdout, stderr))
	cmd.AddCommand(newSystemExtensionInspectCommand(ctx, stdout, false))
	cmd.AddCommand(newSystemExtensionInspectCommand(ctx, stdout, true))
	cmd.AddCommand(newSystemExtensionPublishCommand(ctx, stdout))
	return cmd
}

type systemExtensionInspectReport struct {
	Reference            string                             `json:"reference"`
	OCIManifestDigest    string                             `json:"ociManifestDigest"`
	BundleManifestDigest string                             `json:"bundleManifestDigest"`
	Bundle               systemextensionbundle.Bundle       `json:"bundle"`
	Payloads             []systemextensionbundle.Descriptor `json:"payloads"`
}

func newSystemExtensionInspectCommand(ctx context.Context, stdout io.Writer, validate bool) *cobra.Command {
	use := "inspect OCI_REFERENCE"
	short := "Inspect a system extension bundle from an OCI registry"
	if validate {
		use = "validate OCI_REFERENCE"
		short = "Mechanically validate a system extension bundle"
	}
	var architecture, runtimeInterface, output string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			resolved, err := systemextensionbundle.Resolve(ctx, systemextensionbundle.ResolveRequest{
				Reference:        args[0],
				Architecture:     architecture,
				RuntimeInterface: runtimeInterface,
			})
			if err != nil {
				return err
			}
			report := systemExtensionInspectReport{
				Reference:            resolved.Reference,
				OCIManifestDigest:    resolved.OCIManifestDigest,
				BundleManifestDigest: resolved.BundleManifestDigest,
				Bundle:               resolved.Bundle,
			}
			for _, payload := range resolved.Payloads {
				report.Payloads = append(report.Payloads, payload.Descriptor)
			}
			if output == "json" {
				return json.NewEncoder(stdout).Encode(report)
			}
			if output != "text" {
				return fmt.Errorf("--output must be text or json")
			}
			if validate {
				fmt.Fprintf(stdout, "valid system extension bundle %s\n", report.Reference)
			}
			fmt.Fprintf(stdout, "name: %s\nartifact-version: %s\npayload-version: %s\narchitecture: %s\noci-manifest-digest: %s\nbundle-manifest-digest: %s\n",
				report.Bundle.Name, report.Bundle.ArtifactVersion, report.Bundle.PayloadVersion, report.Bundle.Architecture,
				report.OCIManifestDigest, report.BundleManifestDigest)
			for _, payload := range report.Payloads {
				fmt.Fprintf(stdout, "payload: %s %s %s %d\n", payload.Role, payload.FileName, payload.Digest, payload.SizeBytes)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&architecture, "architecture", "", "optional target architecture compatibility check")
	cmd.Flags().StringVar(&runtimeInterface, "runtime-interface", "", "optional target KatlOS runtime interface compatibility check")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}

func newSystemExtensionPublishCommand(ctx context.Context, stdout io.Writer) *cobra.Command {
	var ref, name, artifactVersion, payloadVersion, architecture, createdAt string
	var runtimeInterfaces, sysextPaths, confextPaths []string
	var packOnly bool
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Build and immutably publish a system extension OCI bundle",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			created, err := time.Parse(time.RFC3339, createdAt)
			if err != nil || created.Format(time.RFC3339) != createdAt {
				return fmt.Errorf("--created-at must be canonical RFC3339")
			}
			inputs := make([]systemextensionbundle.Input, 0, len(sysextPaths)+len(confextPaths))
			for _, inputPath := range sysextPaths {
				inputs = append(inputs, systemextensionbundle.Input{
					Path: inputPath, Role: systemextensionbundle.SysextRole,
					FileName: filepath.Base(inputPath),
				})
			}
			for _, inputPath := range confextPaths {
				inputs = append(inputs, systemextensionbundle.Input{
					Path: inputPath, Role: systemextensionbundle.ConfextRole,
					FileName: filepath.Base(inputPath),
				})
			}
			build := systemextensionbundle.BuildRequest{
				Name: name, ArtifactVersion: artifactVersion, PayloadVersion: payloadVersion,
				Architecture: architecture, SupportedRuntimeInterfaces: runtimeInterfaces,
				CreatedAt: created, Payloads: inputs,
			}
			annotations := map[string]string{
				ocispec.AnnotationTitle:       "KatlOS " + name + " system extension",
				ocispec.AnnotationDescription: "System extension bundle produced with Katl's shared payload-bundle publisher",
			}
			if packOnly {
				packed, built, err := systemextensionbundle.Pack(ctx, build, annotations)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "packed: %s\nbundle-manifest-digest: %s\n", packed.ManifestDigest, built.ManifestHash)
				return nil
			}
			if strings.TrimSpace(ref) == "" {
				return fmt.Errorf("--ref is required unless --pack-only is used")
			}
			published, built, err := systemextensionbundle.Publish(ctx, systemextensionbundle.PublishRequest{
				Reference: ref, Build: build, Annotations: annotations,
			})
			if err != nil {
				return err
			}
			action := "published"
			if published.Existing {
				action = "already-published"
			}
			fmt.Fprintf(stdout, "%s: %s@%s\nbundle-manifest-digest: %s\n",
				action, ref, published.ManifestDigest, built.ManifestHash)
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "OCI destination REGISTRY/REPOSITORY:TAG")
	cmd.Flags().StringVar(&name, "name", "", "extension bundle name")
	cmd.Flags().StringVar(&artifactVersion, "artifact-version", "", "immutable producer artifact version")
	cmd.Flags().StringVar(&payloadVersion, "payload-version", "", "application payload version")
	cmd.Flags().StringVar(&architecture, "architecture", "", "systemd extension architecture")
	cmd.Flags().StringSliceVar(&runtimeInterfaces, "runtime-interface", nil, "compatible KatlOS runtime interface (repeatable)")
	cmd.Flags().StringSliceVar(&sysextPaths, "sysext", nil, "sysext raw image path (repeatable)")
	cmd.Flags().StringSliceVar(&confextPaths, "confext", nil, "confext raw image path (repeatable)")
	cmd.Flags().StringVar(&createdAt, "created-at", "", "reproducible creation timestamp in RFC3339")
	cmd.Flags().BoolVar(&packOnly, "pack-only", false, "pack and validate the OCI envelope without contacting a registry")
	for _, flagName := range []string{"name", "artifact-version", "payload-version", "architecture", "runtime-interface", "sysext", "created-at"} {
		_ = cmd.MarkFlagRequired(flagName)
	}
	return cmd
}

func newSystemExtensionStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	_ = stderr
	opts := systemExtensionStatusOptions{timeout: 15 * time.Second, output: "text"}
	cmd := &cobra.Command{
		Use:   "status [NAME]",
		Short: "Show generic extension, payload, configuration, and unit state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = strings.TrimSpace(args[0])
			}
			return runSystemExtensionStatus(ctx, opts, name, stdout)
		},
	}
	addManagementTargetFlags(cmd, &opts.target)
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "management request timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func runSystemExtensionStatus(ctx context.Context, opts systemExtensionStatusOptions, selectedName string, stdout io.Writer) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	target, err := resolveManagementTarget(opts.target)
	if err != nil {
		return err
	}
	node := hostTargetName(target)
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		return fmt.Errorf("connect to %s at %s: %w", node, target.endpoint, err)
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return fmt.Errorf("read status from %s: %w", node, err)
	}
	extensions := status.GetSystemExtensions()
	if selectedName != "" {
		filtered := extensions[:0]
		for _, extension := range extensions {
			if extension.GetName() == selectedName {
				filtered = append(filtered, extension)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("node %s has no desired or observed system extension named %q", node, selectedName)
		}
		extensions = filtered
	}
	report := systemExtensionStatusReport{Node: node, Extensions: extensions}
	if opts.output == "json" {
		return json.NewEncoder(stdout).Encode(report)
	}
	return writeSystemExtensionStatus(stdout, report)
}

func writeSystemExtensionStatus(stdout io.Writer, report systemExtensionStatusReport) error {
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESIRED\tSTAGED\tACTIVE\tCOMPATIBILITY\tGENERATION\tREBOOT")
	for _, extension := range report.Extensions {
		generationID := extension.GetObservedGenerationId()
		if extension.GetRebootRequired() {
			generationID = extension.GetObservedGenerationId() + " -> " + extension.GetDesiredGenerationId()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			extension.GetName(), extension.GetDesiredState(), extension.GetStagingState(), extension.GetActivationState(),
			extension.GetCompatibility(), generationID, yesNo(extension.GetRebootRequired()))
		if extension.GetSubmittedReference() != "" {
			fmt.Fprintf(w, "\tbundle\t%s\n", extension.GetSubmittedReference())
			fmt.Fprintf(w, "\tdigests\toci=%s bundle=%s\n", extension.GetOciManifestDigest(), extension.GetBundleManifestDigest())
		}
		for _, payload := range extension.GetPayloads() {
			fmt.Fprintf(w, "\tpayload\t%s %s selected=%s active=%s digest=%s\n",
				payload.GetRole(), payload.GetName(), yesNo(payload.GetSelected()), yesNo(payload.GetActive()), payload.GetDigest())
		}
		for _, file := range extension.GetFiles() {
			fmt.Fprintf(w, "\tfile\t%s mode=%#o sha256=%s\n", file.GetPath(), file.GetMode(), file.GetSha256())
		}
		for _, unit := range extension.GetUnits() {
			fmt.Fprintf(w, "\tunit\t%s enabled=%s boot-health=%s %s/%s result=%s\n",
				unit.GetName(), yesNo(unit.GetEnable()), yesNo(unit.GetRequiredForBootHealth()),
				unit.GetActiveState(), unit.GetSubState(), unit.GetResult())
			if unit.GetFailureDiagnostic() != "" {
				fmt.Fprintf(w, "\t\t%s\n", strings.ReplaceAll(unit.GetFailureDiagnostic(), "\n", "\n\t\t"))
			}
		}
	}
	return w.Flush()
}
