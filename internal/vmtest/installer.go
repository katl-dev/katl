package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type InstallerBootConfig struct {
	InstallerUKI    string
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
	vm.Boot = VMBoot{UKI: config.InstallerUKI}
	return vmRunner.Run(ctx, result, vm)
}

func checkInstallerBoot(config InstallerBootConfig) error {
	if config.InstallerUKI == "" {
		return errors.New("installer UKI is required")
	}
	if _, err := os.Stat(config.InstallerUKI); err != nil {
		return fmt.Errorf("installer UKI not found: %w", err)
	}
	if config.RuntimeArtifact != "" {
		if _, err := os.Stat(config.RuntimeArtifact); err != nil {
			return fmt.Errorf("runtime artifact not found: %w", err)
		}
	}
	return nil
}
