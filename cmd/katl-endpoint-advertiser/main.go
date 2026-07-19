package main

import (
	"context"
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
		return withdraw(context.Background())
	}
	file, err := os.Open(*configPath)
	if err != nil {
		return fmt.Errorf("open generated config: %w", err)
	}
	object, err := bgpapivip.Decode(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	config, err := bgpapivip.Normalize(object.Spec)
	if err != nil {
		return err
	}
	interval, err := time.ParseDuration(config.Health.Interval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("invalid generated health interval %q", config.Health.Interval)
	}
	client := bgpapivip.CommandBirdClient{Config: config}
	controller := bgpapivip.Controller{
		Config:            config,
		AppPayloadVersion: version,
		Bird:              client,
		Interface:         bgpapivip.LinuxInterfaceChecker{},
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

func withdraw(parent context.Context) error {
	return withdrawWith(parent, bgpapivip.CommandBirdClient{}, bgpapivip.ExecRunner{})
}

func withdrawWith(parent context.Context, client bgpapivip.BirdClient, runner bgpapivip.CommandRunner) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := client.SetAdvertisement(ctx, false); err == nil {
		return nil
	}
	output, err := runner.Output(ctx, "systemctl", "stop", "katl-app-bird.service")
	if err != nil {
		return fmt.Errorf("withdraw route and stop routing daemon: %s", string(output))
	}
	return nil
}
