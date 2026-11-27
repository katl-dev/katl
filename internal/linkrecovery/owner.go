package linkrecovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/deadline"
	"github.com/katl-dev/katl/internal/managementnetwork"
)

// owner reconfigures each required link at most once per boot, after
// systemd-networkd has reported it without carrier, while sysfs reports
// carrier up, for the whole grace period. That state can begin at any time
// after the link comes up. A link that sysfs also reports without carrier is
// left alone.
type owner struct {
	// armGrace starts the grace period for one link. The returned context
	// ends with context.DeadlineExceeded when the period elapses.
	armGrace func(ctx context.Context, link string) (context.Context, context.CancelFunc, error)
	// gracePeriod is how long armGrace's period lasts, for the event that
	// reports a grace period starting.
	gracePeriod time.Duration
	reconfigure func(ctx context.Context, link string) error
	dir         string
	events      io.Writer
	now         func() time.Time

	graces  map[string]grace
	settled map[string]bool
}

type grace struct {
	ctx  context.Context
	stop context.CancelFunc
}

// Run watches the required links until ctx ends. read returns systemd-networkd
// link state as 'networkctl list --json=short' prints it.
func Run(ctx context.Context, read func(context.Context) ([]byte, error), events io.Writer) error {
	gracePeriod, err := deadline.Period(deadline.ManagementCarrier)
	if err != nil {
		return err
	}
	o := &owner{
		armGrace: func(ctx context.Context, link string) (context.Context, context.CancelFunc, error) {
			return deadline.Arm(ctx, deadline.ManagementCarrier, link)
		},
		gracePeriod: gracePeriod,
		reconfigure: networkctlReconfigure,
		dir:         Dir,
		events:      events,
		now:         func() time.Time { return time.Now().UTC() },
	}
	defer o.stop()

	backoff := managementnetwork.PollBackoff()
	lastErr := ""
	for {
		observation, err := managementnetwork.Observe(ctx, read)
		switch {
		case err != nil && ctx.Err() == nil:
			// Unreadable state neither starts nor ends a grace period.
			if err.Error() != lastErr {
				fmt.Fprintf(events, "cannot read required link state: %v\n", err)
				lastErr = err.Error()
			}
		case err == nil:
			lastErr = ""
			o.observe(ctx, observation.CarrierDisagreements)
		}
		timer := time.NewTimer(backoff.Next())
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// observe updates the grace periods from the links that currently disagree
// and reconfigures each link whose grace period elapsed while it disagreed.
func (o *owner) observe(ctx context.Context, disagreements []string) {
	if o.graces == nil {
		o.graces = map[string]grace{}
		o.settled = map[string]bool{}
	}
	for link, grace := range o.graces {
		if !slices.Contains(disagreements, link) {
			grace.stop()
			delete(o.graces, link)
		}
	}
	for _, link := range disagreements {
		if o.settled[link] {
			continue
		}
		grace, armed := o.graces[link]
		if !armed {
			// An earlier run of the owner in this boot made the attempt.
			if recorded(o.dir, link) {
				o.settled[link] = true
				continue
			}
			o.arm(ctx, link)
			continue
		}
		if grace.ctx.Err() == nil {
			continue
		}
		grace.stop()
		delete(o.graces, link)
		if errors.Is(context.Cause(grace.ctx), context.DeadlineExceeded) {
			o.settled[link] = true
			o.attempt(ctx, link)
		}
	}
}

func (o *owner) arm(ctx context.Context, link string) {
	ctx, stop, err := o.armGrace(ctx, link)
	if err != nil {
		o.settled[link] = true
		fmt.Fprintf(o.events, "required link %s: %s; cannot start the grace period, so not reconfiguring it in this run: %v\n", link, presentSymptom, err)
		return
	}
	o.graces[link] = grace{ctx: ctx, stop: stop}
	fmt.Fprintf(o.events, "required link %s: %s; reconfiguring once if this persists for %s\n", link, presentSymptom, o.gracePeriod)
}

func (o *owner) attempt(ctx context.Context, link string) {
	record := Record{Link: link, At: o.now(), Outcome: Attempted}
	claimed, err := claim(o.dir, record)
	if err != nil {
		fmt.Fprintf(o.events, "required link %s: not reconfiguring after %s: %v\n", link, symptom, err)
		return
	}
	if !claimed {
		return
	}

	if err := o.reconfigure(ctx, link); err != nil {
		record.Outcome, record.Error = Failed, err.Error()
	} else {
		record.Outcome = Reconfigured
	}
	if err := update(o.dir, record); err != nil {
		fmt.Fprintf(o.events, "%s; recording the result failed: %v\n", record, err)
		return
	}
	fmt.Fprintln(o.events, record)
}

func (o *owner) stop() {
	for link, grace := range o.graces {
		grace.stop()
		delete(o.graces, link)
	}
}

// networkctlReconfigure leaves the link out of its error, which the record
// already names.
func networkctlReconfigure(ctx context.Context, link string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "networkctl", "reconfigure", link).CombinedOutput()
	if err != nil {
		return fmt.Errorf("networkctl reconfigure: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
