// Command katl-vmtest-health records host health around a VM test run.
//
//	katl-vmtest-health start -out start.json
//	katl-vmtest-health finish -start start.json -run-dir DIR -go-test-log LOG -status pass|fail -out host-health.json
//
// It is diagnostic: it never changes a run's exit status.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/katl-dev/katl/internal/vmtest/hosthealth"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "katl-vmtest-health:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: katl-vmtest-health start|finish [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	out := fs.String("out", "", "output JSON file")
	startFile := fs.String("start", "", "sample written by start")
	runDir := fs.String("run-dir", "", "run directory holding serial logs")
	goTestLog := fs.String("go-test-log", "", "go test log")
	status := fs.String("status", "", "pass or fail")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	switch args[0] {
	case "start":
		s, err := hosthealth.ReadSample("/proc", time.Now().UTC())
		if err != nil {
			return err
		}
		return hosthealth.WriteJSON(*out, s)
	case "finish":
		if *status != "pass" && *status != "fail" {
			return fmt.Errorf("-status must be pass or fail")
		}
		start, err := hosthealth.ReadSampleFile(*startFile)
		if err != nil {
			return err
		}
		r, err := hosthealth.Finish(start, "/proc", *runDir, *goTestLog, *status == "pass")
		if err != nil {
			return err
		}
		if err := hosthealth.WriteJSON(*out, r); err != nil {
			return err
		}
		h := r.Health
		fmt.Printf("vmtest host health: %s (%s); steal %.1f%%, cpu pressure %.1f%%, memory pressure %.1f%%, load %.2f, guest clocksource watchdog %d, guest stalls %d\n",
			r.Verdict.Outcome, r.Verdict.Reason, h.StealRatio*100, h.CPUPressure*100, h.MemoryPressure*100, h.Load1End, h.ClocksourceWatch, h.GuestStalls)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}
