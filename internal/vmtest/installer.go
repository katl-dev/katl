package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type InstallerBootConfig struct {
	InstallerUKI    string
	InstallerKernel string
	InstallerInitrd string
	CommandLine     []string
	RuntimeArtifact string
	Expect          string
	VM              VMConfig
}

func RunInstallerBoot(ctx context.Context, runner Runner, scenario Scenario, config InstallerBootConfig, vmRunner VMRunner) (Result, error) {
	result, err := runner.Plan(scenario)
	if err != nil {
		return Result{}, err
	}
	result = BootInstaller(ctx, result, config, vmRunner)
	if err := runner.Write(scenario, result); err != nil {
		return result, err
	}
	return result, nil
}

func BootInstaller(ctx context.Context, result Result, config InstallerBootConfig, vmRunner VMRunner) Result {
	if err := checkInstallerBoot(config); err != nil {
		return finishVM(result, "installer", StatusFailed, err.Error(), runnerTime(), runnerTime())
	}
	vm := config.VM
	vm.Phase = "installer"
	vm.Expect = first(vm.Expect, config.Expect, "Katl installer ready")
	if config.InstallerKernel != "" {
		vm.Boot = VMBoot{
			Kernel:      config.InstallerKernel,
			Initrd:      config.InstallerInitrd,
			CommandLine: config.CommandLine,
		}
	} else {
		vm.Boot = VMBoot{UKI: config.InstallerUKI}
	}
	return vmRunner.Run(ctx, result, vm)
}

func checkInstallerBoot(config InstallerBootConfig) error {
	if config.InstallerKernel != "" || config.InstallerInitrd != "" {
		if config.InstallerKernel == "" {
			return errors.New("installer kernel is required when installer initrd is set")
		}
		if config.InstallerInitrd == "" {
			return errors.New("installer initrd is required when installer kernel is set")
		}
		if _, err := os.Stat(config.InstallerKernel); err != nil {
			return fmt.Errorf("installer kernel not found: %w", err)
		}
		if _, err := os.Stat(config.InstallerInitrd); err != nil {
			return fmt.Errorf("installer initrd not found: %w", err)
		}
	} else if config.InstallerUKI == "" {
		return errors.New("installer UKI or kernel/initrd is required")
	} else if _, err := os.Stat(config.InstallerUKI); err != nil {
		return fmt.Errorf("installer UKI not found: %w", err)
	}
	if config.RuntimeArtifact != "" {
		if _, err := os.Stat(config.RuntimeArtifact); err != nil {
			return fmt.Errorf("runtime artifact not found: %w", err)
		}
	}
	return nil
}
