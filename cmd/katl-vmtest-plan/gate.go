package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/katl-dev/katl/internal/flavour"
)

type gateGroup struct {
	name string
	args []string
}

func runGate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("vmtest-run gate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runner := flags.String("runner", "", "path to scripts/vmtest-run")
	id := flags.String("id", "", "run ID prefix")
	flavours := flags.String("flavour", "", "comma-separated kernel flavours")
	affected := flags.Bool("affected", false, "test affected CI groups")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runner == "" || *id == "" {
		return errors.New("missing VM test runner or run ID")
	}
	if *flavours == "" && *affected {
		*flavours = strings.Join(kernelFlavours, ",")
	}
	var selected []string
	for _, value := range strings.Split(*flavours, ",") {
		normalized, err := flavour.Normalize(value)
		if err != nil || value == "" || slices.Contains(selected, normalized) {
			return errors.New("--flavour requires standard, lts, or both without duplicates")
		}
		selected = append(selected, normalized)
	}
	for _, name := range []string{"KATL_VMTEST_RUN_ID", "KATL_VMTEST_RUN_DIR", "KATL_VMTEST_LIBVIRT_DOMAIN_PREFIX", "KATL_MKOSI_BUILD_DIR", "KATL_MKOSI_CACHE_DIR", "KATL_VMTEST_CACHE_DIR"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("unset %s to let the gate allocate paths and identities", name)
		}
	}

	jobs := []gateGroup{{args: flags.Args()}}
	if *affected {
		if flags.NArg() != 0 {
			return errors.New("--affected supplies the Go test arguments from the CI plan")
		}
		var err error
		jobs, err = affectedGateGroups()
		if err != nil {
			return err
		}
	}
	if len(jobs) == 0 {
		fmt.Fprintln(stdout, "vmtest gate: no affected groups")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	type result struct {
		summary string
		err     error
	}
	results := make([]result, len(selected))
	var workers sync.WaitGroup
	for i, kernel := range selected {
		workers.Go(func() {
			var summary bytes.Buffer
			err := runFlavour(ctx, *runner, *id+"-"+kernel, kernel, jobs, &summary, stderr)
			results[i] = result{summary.String(), err}
		})
	}
	workers.Wait()

	fmt.Fprintln(stdout, "vmtest gate summary:")
	var errs []error
	for _, outcome := range results {
		fmt.Fprint(stdout, outcome.summary)
		errs = append(errs, outcome.err)
	}
	return errors.Join(errs...)
}

func affectedGateGroups() ([]gateGroup, error) {
	// CI plans from commits, but the runner tests the working tree, so the two
	// agree only on a clean tree.
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("read working tree status: %w", err)
	}
	if len(status) != 0 {
		return nil, errors.New("--affected plans from committed changes, but the working tree has uncommitted or untracked changes; commit them or run git stash -u, or name the tests to run")
	}
	baseOutput, err := exec.Command("git", "merge-base", "origin/main", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("find merge base with origin/main: %w", err)
	}
	base := strings.TrimSpace(string(baseOutput))
	changes, err := exec.Command("git", "diff", "--name-only", "--no-renames", "-z", base, "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("read changes from %s to HEAD: %w", base, err)
	}
	paths, err := readChangedPaths(bytes.NewReader(changes))
	if err != nil {
		return nil, err
	}
	decision, _ := selectVMs(paths, affectedVMs, base)
	var jobs []gateGroup
	for _, group := range batchGroups(decision.groups) {
		jobs = append(jobs, gateGroup{
			name: group.Name,
			args: []string{group.Package, "-run", "^(" + group.Tests + ")$", "-count=1"},
		})
	}
	return jobs, nil
}

// gateDirs are the directories a flavour's runs own. Both lie outside
// _build/vmtest, whose fixture cache the default runner walks, and the fixture
// cache is per flavour because installed-runtime fixtures have one slot per
// node name.
func gateDirs(kernel string) (build, cache string, err error) {
	root, err := filepath.Abs(filepath.Join("_build", "gate", kernel))
	if err != nil {
		return "", "", err
	}
	return filepath.Join(root, "mkosi"), filepath.Join(root, "vmtest"), nil
}

// mkosiLockPath asks the scripts/mkosi next to the runner for the lock that
// guards buildDir, so the lock's location has one owner.
func mkosiLockPath(runner, kernel, buildDir string) (string, error) {
	cmd := exec.Command(filepath.Join(filepath.Dir(runner), "mkosi"), "lock-path")
	cmd.Env = append(os.Environ(), "KATL_FLAVOUR="+kernel, "KATL_MKOSI_BUILD_DIR="+buildDir)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("find the %s build lock: %w", kernel, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func runFlavour(ctx context.Context, runner, id, kernel string, jobs []gateGroup, summary, stderr io.Writer) error {
	fmt.Fprintf(summary, "%s gate %s\n", kernel, id)
	buildDir, cacheDir, err := gateDirs(kernel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	// scripts/mkosi guards a build directory with this lock, and a nested
	// scripts/mkosi that inherits it through KATL_MKOSI_LOCK_FD treats it as
	// held. Holding it for the whole gate keeps other builds out of the outputs
	// until every consumer has finished, including scenarios that read
	// artifacts outside the pin set.
	lockPath, err := mkosiLockPath(runner, kernel, buildDir)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("another gate or build is using %s; wait for it to finish", buildDir)
		}
		return fmt.Errorf("lock %s output directory %s: %w", kernel, buildDir, err)
	}
	if err := lock.Truncate(0); err == nil {
		fmt.Fprintf(lock, "pid %d: scripts/vmtest-run gate %s\n", os.Getpid(), id)
	}

	var errs []error
	for _, job := range jobs {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		runID := id
		if job.name != "" {
			runID += "-" + job.name
		}
		fmt.Fprintf(stderr, "vmtest gate: starting %s (%s)\n", runID, buildDir)
		cmd := exec.CommandContext(ctx, runner, append([]string{"--"}, job.args...)...)
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		// The run inherits the lock as descriptor 3, so an interrupted gate
		// holds it until the child exits.
		cmd.ExtraFiles = []*os.File{lock}
		cmd.Env = append(os.Environ(),
			"KATL_FLAVOUR="+kernel,
			"KATL_MKOSI_BUILD_DIR="+buildDir,
			"KATL_MKOSI_LOCK_FD=3",
			"KATL_VMTEST_CACHE_DIR="+cacheDir,
			"KATL_VMTEST_RUN_ID="+runID,
		)
		cmd.Stdout = summary
		cmd.Stderr = stderr
		fmt.Fprintf(summary, "  run %s\n", runID)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(summary, "  %s: %v\n", runID, err)
			errs = append(errs, fmt.Errorf("%s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}
