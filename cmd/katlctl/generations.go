package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
)

func newGenerationsCommand(ctx context.Context, stdout io.Writer) *cobra.Command {
	command := &cobra.Command{Example: "katlctl node generations list cp-1 --config cluster.yaml", Use: "generations", Short: "List, select and remove node boot generations"}
	for _, action := range []string{"list", "select", "remove"} {
		var targetOptions managementTargetOptions
		var oneShot bool
		output := hostOutputText
		timeout := 30 * time.Second
		use, short := "list [NODE]", "List generations, boot selections and removal protections"
		if action == "select" {
			use = "select GENERATION [NODE]"
			short = "Select the persistent boot default, or use --one-shot for one boot"
		}
		if action == "remove" {
			use = "remove GENERATION [NODE]"
			short = "Remove an unprotected generation and its boot entry"
		}
		example := "katlctl node generations list cp-1 --config cluster.yaml"
		if action != "list" {
			example = "katlctl node generations " + action + " GENERATION cp-1 --config cluster.yaml"
		}
		cmd := &cobra.Command{Use: use, Short: short, Example: example, Args: cobra.MaximumNArgs(1)}
		if action != "list" {
			cmd.Args = cobra.RangeArgs(1, 2)
		}
		cmd.RunE = func(_ *cobra.Command, args []string) error {
			id := ""
			if action != "list" {
				id = args[0]
				args = args[1:]
			}
			if err := selectHostNode(&targetOptions.nodeName, args); err != nil {
				return err
			}
			session, err := openManagementSession(ctx, targetOptions, timeout)
			if err != nil {
				return err
			}
			defer session.close()
			state, err := session.client.GetNodeStatus(session.ctx, &agentapi.GetNodeStatusRequest{})
			if err != nil {
				return err
			}
			if err := bindManagementStatus(&session.target, state); err != nil {
				return err
			}
			if action == "list" {
				result, err := session.client.ListGenerations(session.ctx, &agentapi.ListGenerationsRequest{})
				if err != nil {
					return fmt.Errorf("list generations: %w", err)
				}
				if output == hostOutputJSON {
					return writeProtoJSON(stdout, "generation list", result)
				}
				w := newTable(stdout)
				w.row("GENERATION", "KATLOS", "SLOT", "CREATED", "BOOT", "PROTECTED BY")
				for _, gen := range result.Generations {
					boot := "available"
					if gen.UnavailableReason != "" {
						boot = gen.UnavailableReason
					}
					if gen.GenerationId == result.NextBootGenerationId {
						boot = "next boot"
						if result.OneShot {
							boot += " (one-shot)"
						}
					}
					w.row(gen.GenerationId, gen.RuntimeVersion, gen.RootSlot, gen.CreatedAt, boot, strings.Join(gen.ProtectedBy, ", "))
				}
				if err := w.flush(); err != nil {
					return err
				}
				if result.KeepLast > 0 {
					_, err = fmt.Fprintf(stdout, "Retention per OS version: newest %d generations plus those younger than %s; protected generations are always kept.\n", result.KeepLast, result.MaxAge)
				}
				return err
			}
			req := &agentapi.GenerationMutationRequest{GenerationId: id, OneShot: oneShot, ExpectedEnrollmentId: state.EnrollmentId, ExpectedInventoryNodeName: state.InventoryNodeName, ExpectedMachineId: state.MachineId, ExpectedCurrentGenerationId: state.CurrentGenerationId}
			var result *agentapi.GenerationMutationResult
			if action == "select" {
				result, err = session.client.SelectGeneration(session.ctx, req)
			} else {
				result, err = session.client.RemoveGeneration(session.ctx, req)
			}
			if err != nil {
				return fmt.Errorf("%s generation %s: %w", action, id, err)
			}
			if output == hostOutputJSON {
				return writeProtoJSON(stdout, "generation "+action, result)
			}
			if action == "remove" {
				_, err = fmt.Fprintf(stdout, "Generation %s removed.\n", id)
				return err
			}
			mode := "persistent default"
			if oneShot {
				mode = "one-shot boot"
			}
			_, err = fmt.Fprintf(stdout, "Generation %s selected for %s. Reboot with katlctl node reboot.\n", id, mode)
			return err
		}
		addManagementTargetFlags(cmd, &targetOptions)
		cmd.Flags().DurationVar(&timeout, "timeout", timeout, "management request timeout")
		addOutputFlag(cmd, &output, output, "text", "json")
		if action == "select" {
			cmd.Flags().BoolVar(&oneShot, "one-shot", false, "boot this generation once, preserving the persistent default")
		}
		command.AddCommand(cmd)
	}
	return command
}
