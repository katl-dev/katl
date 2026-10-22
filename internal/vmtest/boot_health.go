package vmtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// WaitBootComplete observes the current boot's promotion point. Persisted
// generation health can still describe a previous boot before this target runs.
func (g *GuestControl) WaitBootComplete(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := g.Systemctl(ctx, "is-active", "--quiet", "katl-boot-complete.target")
		if err == nil {
			return checkBootHealth(ctx, g)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for boot completion: %w (last observation: %v)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func checkBootHealth(ctx context.Context, guest *GuestControl) error {
	for _, unit := range []string{"katl-boot-complete.target", "sshd.service", "katlc-agent.service"} {
		if _, err := guest.Systemctl(ctx, "is-active", "--quiet", unit); err != nil {
			return fmt.Errorf("boot access service %s: %w", unit, err)
		}
	}
	health, err := guest.Systemctl(ctx, "show", "--property=Result", "--value", "katl-boot-health.service")
	if err != nil {
		return err
	}
	result, err := os.ReadFile(health.Stdout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != "success" {
		return fmt.Errorf("boot-health result = %q", strings.TrimSpace(string(result)))
	}
	return nil
}
