package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

var version = "0.0.0-dev"

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katl endpoint advertiser: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	command := "run"
	if len(args) > 0 && args[0] == "withdraw" {
		command = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("katl-endpoint-advertiser", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", bgpapivip.ConfigPath, "generated endpoint configuration")
	statusPath := flags.String("status", bgpapivip.LiveStatusPath, "bounded endpoint status path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	if command == "withdraw" {
		return withdraw(context.Background(), *configPath)
	}
	config, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	interval, err := time.ParseDuration(config.Health.Interval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("invalid generated health interval %q", config.Health.Interval)
	}
	client := bgpapivip.CommandBirdClient{Config: config}
	owner := bgpapivip.NetlinkVIPOwner{}
	controller := bgpapivip.Controller{
		Config:            config,
		AppPayloadVersion: version,
		Bird:              client,
		Interface:         bgpapivip.LinuxInterfaceChecker{},
		Owner:             owner,
		Writer:            bgpapivip.FileStatusWriter{LivePath: *statusPath},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastState := ""
	for {
		status, runErr := controller.RunOnce(ctx)
		state := status.AdvertisementState + "/" + status.WithdrawReason
		if state != lastState || runErr != nil {
			if runErr != nil {
				fmt.Fprintf(stderr, "endpoint state=%s health=%s: %v\n", state, status.HealthState, runErr)
			} else {
				fmt.Fprintf(stderr, "endpoint state=%s health=%s\n", state, status.HealthState)
			}
			lastState = state
		}
		if runErr != nil {
			return failClosed(runErr, owner, config, bgpapivip.ExecRunner{})
		}
		select {
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := controller.Stop(stopCtx)
			return err
		case <-ticker.C:
		}
	}
}

func loadConfig(path string) (bgpapivip.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return bgpapivip.Config{}, fmt.Errorf("open generated config: %w", err)
	}
	object, err := bgpapivip.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return bgpapivip.Config{}, err
	}
	if closeErr != nil {
		return bgpapivip.Config{}, closeErr
	}
	config, err := bgpapivip.Normalize(object.Spec)
	if err != nil {
		return bgpapivip.Config{}, err
	}
	return config, nil
}

func failClosed(runErr error, owner bgpapivip.VIPOwner, config bgpapivip.Config, runner bgpapivip.CommandRunner) error {
	if runErr == nil {
		return nil
	}
	return errors.Join(runErr, withdrawWith(context.Background(), owner, config, runner))
}

func withdraw(parent context.Context, configPath string) error {
	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	return withdrawWith(parent, bgpapivip.NetlinkVIPOwner{}, config, bgpapivip.ExecRunner{})
}

func withdrawWith(parent context.Context, owner bgpapivip.VIPOwner, config bgpapivip.Config, runner bgpapivip.CommandRunner) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	releaseErr := owner.SetOwned(ctx, config, false)
	if releaseErr == nil {
		return nil
	}
	output, stopErr := runner.Output(ctx, "systemctl", "stop", "katl-app-bird.service")
	if stopErr != nil {
		stopErr = fmt.Errorf("stop routing daemon after local endpoint release failed: %s", string(output))
	}
	return errors.Join(fmt.Errorf("release local endpoint address: %w", releaseErr), stopErr)
}
