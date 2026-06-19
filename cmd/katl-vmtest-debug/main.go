package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/zariel/katl/internal/vmtest"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: katl-vmtest-debug <run-dir|result.json>")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		var exitErr exitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(int(exitErr))
		}
		fmt.Fprintf(os.Stderr, "vmtest-debug: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		usage()
		return exitCodeError(2)
	}
	resultFiles, err := vmtest.FindDebugResultFiles(args[0])
	if err != nil {
		return err
	}
	reports, err := vmtest.LoadDebugTargetReports(resultFiles)
	if err != nil {
		return err
	}
	if len(reports) == 0 {
		return fmt.Errorf("no vmtest domains recorded in %s", args[0])
	}
	return vmtest.WriteDebugTargetReport(os.Stdout, reports)
}

type exitCodeError int

func (e exitCodeError) Error() string {
	return fmt.Sprintf("exit %d", int(e))
}
