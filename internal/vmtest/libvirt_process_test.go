package vmtest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLibvirtConsoleProcess(t *testing.T) {
	mode := os.Getenv("KATL_CONSOLE_PROCESS")
	if mode == "" {
		return
	}
	root := os.Getenv("KATL_CONSOLE_FIXTURE")
	if mode == "owner" {
		serial, err := os.OpenFile(filepath.Join(root, "serial.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			panic(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e := LibvirtVMExecutor{VirshPath: filepath.Join(root, "virsh"), DomainName: "fixture"}
		_ = e.Run(ctx, "", nil, serial)
		_ = serial.Close()
		os.Exit(0)
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	if args[0] == "-c" {
		args = args[2:]
	}
	switch args[0] {
	case "define", "start", "destroy", "undefine":
		os.Exit(0)
	case "domstate":
		_, _ = os.Stdout.WriteString("running\n")
		os.Exit(0)
	case "console":
		if _, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS); err != nil {
			panic(err)
		}
		terminal, err := os.Open("/dev/tty")
		if err != nil {
			panic(err)
		}
		_ = terminal.Close()
		terminalPath, err := os.Readlink("/proc/self/fd/0")
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(root, "terminal"), []byte(terminalPath), 0o600); err != nil {
			panic(err)
		}
		// Observe stdin hangup directly, including when the owner cannot run
		// cleanup. Native virsh also exits when its terminal input closes.
		signal.Ignore(syscall.SIGHUP)
		_, _ = os.Stdout.WriteString(os.Getenv("KATL_CONSOLE_MARKER") + "\n")
		_, _ = os.Stderr.WriteString("console stderr\n")
		if err := os.WriteFile(filepath.Join(root, "console.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			panic(err)
		}
		var input [1]byte
		if n, err := os.Stdin.Read(input[:]); n != 0 || err == nil {
			os.Exit(41)
		}
		if err := os.WriteFile(filepath.Join(root, "disconnected"), nil, 0o600); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	os.Exit(40)
}

func consoleFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATL_CONSOLE_FIXTURE", root)
	t.Setenv("KATL_CONSOLE_BINARY", binary)
	t.Setenv("KATL_CONSOLE_MARKER", "console ready")
	writeExecutable(t, filepath.Join(root, "virsh"), `#!/usr/bin/env bash
export KATL_CONSOLE_PROCESS=virsh
exec "$KATL_CONSOLE_BINARY" -test.run='^TestLibvirtConsoleProcess$' -- "$@"
`)
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(root, "console.pid")); err == nil {
			pid, _ := strconv.Atoi(string(b))
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return root
}

func waitConsoleFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(path); err == nil {
			return b
		}
		select {
		case <-deadline.C:
			t.Fatalf("console did not produce %s", filepath.Base(path))
		case <-tick.C:
		}
	}
}

func TestLibvirtConsoleCancellation(t *testing.T) {
	root := consoleFixture(t)
	serial, err := os.Create(filepath.Join(root, "serial.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer serial.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := cancelWhenFileExists(t, ctx, cancel, filepath.Join(root, "console.pid"))
	e := LibvirtVMExecutor{VirshPath: filepath.Join(root, "virsh"), DomainName: "fixture"}
	err = e.Run(ctx, "", nil, serial)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled console run = %v", err)
	}
	pid, err := strconv.Atoi(string(waitConsoleFile(t, filepath.Join(root, "console.pid"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("console was not reaped after cancellation: %v", err)
	}
}

func TestLibvirtConsoleOwnerExit(t *testing.T) {
	root := consoleFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestLibvirtConsoleProcess$")
	cmd.Env = append(os.Environ(), "KATL_CONSOLE_PROCESS=owner")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitConsoleFile(t, filepath.Join(root, "console.pid"))
	terminal := string(waitConsoleFile(t, filepath.Join(root, "terminal")))
	if _, err := os.Stat(terminal); err != nil {
		t.Fatalf("console has no live PTY before owner exit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "disconnected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("console disconnected while its owner was active: %v", err)
	}

	// Kill only the owner. The console must observe kernel terminal hangup
	// without a signal or cleanup action directed at the console process.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed owner succeeded")
	}
	waitConsoleFile(t, filepath.Join(root, "disconnected"))
}

func TestVMConsoleAppend(t *testing.T) {
	root := consoleFixture(t)
	result, config := vmFixture(t)
	config.VirshPath = filepath.Join(root, "virsh")
	config.Timeout = 5 * time.Second
	runner := VMRunner{probe: probe{
		stat:   os.Stat,
		access: func(string) error { return nil },
	}}
	if err := os.MkdirAll(filepath.Dir(result.Artifacts.InstallerSerial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.Artifacts.InstallerSerial, []byte("prior boot\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, marker := range []string{"first boot", "second boot"} {
		t.Setenv("KATL_CONSOLE_MARKER", marker)
		config.Expect = marker
		updated := runner.Run(context.Background(), result, config)
		if updated.Status != StatusPassed {
			t.Fatalf("VM console run: %s", updated.FailureSummary)
		}
	}

	serial, err := os.ReadFile(result.Artifacts.InstallerSerial)
	if err != nil {
		t.Fatal(err)
	}
	text := string(serial)
	if !strings.HasPrefix(text, "prior boot\n") || strings.Count(text, "first boot\n") != 1 || strings.Count(text, "second boot\n") != 1 || strings.Count(text, "console stderr\n") != 2 {
		t.Fatalf("serial append across console runs = %q", text)
	}
}

func TestLibvirtCommandProcess(t *testing.T) {
	mode := os.Getenv("KATL_COMMAND_PROCESS")
	if mode == "" {
		return
	}
	root := os.Getenv("KATL_COMMAND_FIXTURE")
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	go func() {
		<-interrupted
		if mode == "virsh" {
			_ = os.WriteFile(filepath.Join(root, "interrupted"), nil, 0o600)
		}
		os.Exit(130)
	}()
	if mode == "owner" {
		e := LibvirtVMExecutor{VirshPath: filepath.Join(root, "virsh"), DomainName: "fixture"}
		_ = e.Run(context.Background(), "", nil, nil)
		os.Exit(0)
	}
	if err := os.WriteFile(filepath.Join(root, "command.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		panic(err)
	}
	select {}
}

func TestLibvirtCommandInterrupt(t *testing.T) {
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(root, "virsh"), `#!/usr/bin/env bash
export KATL_COMMAND_PROCESS=virsh
exec "$KATL_COMMAND_BINARY" -test.run='^TestLibvirtCommandProcess$' -- "$@"
`)
	cmd := exec.Command(binary, "-test.run=^TestLibvirtCommandProcess$")
	cmd.Env = append(os.Environ(), "KATL_COMMAND_PROCESS=owner", "KATL_COMMAND_FIXTURE="+root, "KATL_COMMAND_BINARY="+binary)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if b, err := os.ReadFile(filepath.Join(root, "command.pid")); err == nil {
			pid, _ := strconv.Atoi(string(b))
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := filepath.Join(root, "command.pid")
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("libvirt command did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("executor owner after terminal interrupt = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "interrupted")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("terminal interrupt left the libvirt command running")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestLibvirtCommandCancellation(t *testing.T) {
	root := t.TempDir()
	virsh := filepath.Join(root, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
printf '%s' "$$" >"$KATL_COMMAND_PID"
exec sleep 60
`)
	pidFile := filepath.Join(root, "command.pid")
	t.Setenv("KATL_COMMAND_PID", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := cancelWhenFileExists(t, ctx, cancel, pidFile)
	err := (LibvirtVMExecutor{VirshPath: virsh, DomainName: "fixture"}).Run(ctx, "", nil, nil)
	<-done
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("cancelled libvirt command = %v", err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("libvirt command survived cancellation: %v", err)
	}
}
