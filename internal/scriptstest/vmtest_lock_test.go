package scriptstest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper is an actual process beneath the runner, with native inherited
// descriptors and signals. No libvirt connection or VM is needed.
func TestVMTestLockProcess(t *testing.T) {
	mode := os.Getenv("KATL_LOCK_PROCESS")
	if mode == "" {
		return
	}
	root := os.Getenv("KATL_LOCK_FIXTURE")
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	go func() { <-signals; os.Exit(130) }()
	barrier := func(name string) {
		f, err := os.OpenFile(filepath.Join(root, name+".fifo"), os.O_RDWR, 0)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		if err := os.WriteFile(filepath.Join(root, name+".ready"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			panic(err)
		}
		var b [1]byte
		if _, err := f.Read(b[:]); err != nil {
			panic(err)
		}
	}
	switch mode {
	case "go":
		child := exec.Command("sleep", "30")
		if err := child.Start(); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(root, "descendant.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			panic(err)
		}
		barrier("work")
		code, _ := strconv.Atoi(os.Getenv("KATL_LOCK_EXIT"))
		os.Exit(code)
	case "virsh":
		if len(args) > 1 && args[0] == "-c" {
			args = args[2:]
		}
		switch args[0] {
		case "uri":
			fmt.Println("qemu:///fixture")
		case "net-info":
			fmt.Print("Active: yes\n")
		case "net-dumpxml":
			fmt.Print("<network><ip address='10.77.0.1' prefix='24'/></network>\n")
		case "pool-info":
			fmt.Print("State: running\n")
		case "pool-dumpxml":
			fmt.Printf("<pool><target><path>%s</path></target></pool>\n", root)
		case "list":
			fmt.Println("katl-lock-fixture")
		case "metadata":
			fmt.Print("<vmtest xmlns=\"https://katlos.io/xmlns/vmtest/1\">katl/vmtest</vmtest>\n")
		case "domstate":
			fmt.Println("running")
		case "destroy":
			barrier("cleanup")
		case "undefine":
		default:
			os.Exit(1)
		}
	case "rm":
		barrier("retention")
		if err := exec.Command(os.Getenv("KATL_LOCK_RM"), args...).Run(); err != nil {
			panic(err)
		}
	}
	os.Exit(0)
}

type lockRun struct {
	cmd  *exec.Cmd
	done chan error
	log  string
}

type lockFixture struct {
	root string
	env  []string
}

func newLockFixture(t *testing.T) lockFixture {
	t.Helper()
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cleanup", "work", "retention"} {
		if err := syscall.Mkfifo(filepath.Join(root, name+".fifo"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"go", "virsh", "rm"} {
		writeExecutable(t, filepath.Join(root, mode), "#!/usr/bin/env bash\nexport KATL_LOCK_PROCESS="+mode+"\nexec \"$KATL_LOCK_TEST_BINARY\" -test.run='^TestVMTestLockProcess$' -- \"$@\"\n")
	}
	writeExecutable(t, filepath.Join(root, "tool"), "#!/usr/bin/env bash\nexit 0\n")
	return lockFixture{root: root, env: append(os.Environ(),
		"KATL_LOCK_FIXTURE="+root, "KATL_LOCK_TEST_BINARY="+binary,
		"KATL_VMTEST_GO="+filepath.Join(root, "go"), "KATL_VMTEST_VIRSH="+filepath.Join(root, "virsh"),
		"KATL_VMTEST_IMAGE_TOOL="+filepath.Join(root, "tool"), "KATL_VMTEST_KUBECTL="+filepath.Join(root, "tool"),
		"KATL_VMTEST_RUN_DIR="+filepath.Join(root, "run"), "KATL_VMTEST_CACHE_DIR="+filepath.Join(root, "cache"),
		"KATL_VMTEST_HOST_LOCK="+filepath.Join(root, "host.lock"), "KATL_VMTEST_HOST_LOCK_TIMEOUT=5",
		"KATL_VMTEST_KEEP=always", "KATL_VMTEST_REQUIRE_SCENARIO_RESULT=0")}
}

func (f lockFixture) start(t *testing.T, exit int) lockRun {
	t.Helper()
	log := filepath.Join(f.root, "runner.log")
	out, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "vmtest-run"), "--artifact-set=none", "./internal/vmtest", "-run", "^NoVM$", "-count=1")
	cmd.Env = append(f.env, fmt.Sprintf("KATL_LOCK_EXIT=%d", exit))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		out.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); out.Close() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		for _, name := range []string{"cleanup.ready", "work.ready", "retention.ready", "descendant.pid"} {
			if b, err := os.ReadFile(filepath.Join(f.root, name)); err == nil {
				pid, _ := strconv.Atoi(string(b))
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	})
	return lockRun{cmd: cmd, done: done, log: log}
}

func (f lockFixture) wait(t *testing.T, name string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(filepath.Join(f.root, name)); err == nil {
			return
		}
		select {
		case <-deadline.C:
			log, _ := os.ReadFile(filepath.Join(f.root, "runner.log"))
			t.Fatalf("waiting for %s\n%s", name, log)
		case <-tick.C:
		}
	}
}

func (f lockFixture) release(t *testing.T, name string) {
	t.Helper()
	fd, err := syscall.Open(filepath.Join(f.root, name+".fifo"), syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.Write(fd, []byte{1}); err != nil {
		t.Fatal(err)
	}
}

func (f lockFixture) locked(t *testing.T, want bool) {
	t.Helper()
	err := exec.Command("flock", "--wait", "0.1", filepath.Join(f.root, "host.lock"), "true").Run()
	if want {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("native flock = %v, want contention", err)
		}
	} else if err != nil {
		t.Fatalf("native flock could not acquire released lease: %v", err)
	}
}

func (r lockRun) result(t *testing.T, code int) {
	t.Helper()
	select {
	case err := <-r.done:
		got := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			got = exit.ExitCode()
		}
		if got != code {
			log, _ := os.ReadFile(r.log)
			t.Fatalf("runner exit=%d, want %d\n%s", got, code, log)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not exit")
	}
}

func TestVMTestHostLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
		exit   int
		want   int
	}{
		{name: "success"}, {name: "failure", exit: 7, want: 7},
		{name: "owner-term", signal: syscall.SIGTERM, want: 143},
		{name: "owner-int", signal: syscall.SIGINT, want: 130},
		{name: "owner-kill", signal: syscall.SIGKILL, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLockFixture(t)
			r := f.start(t, tc.exit)
			f.wait(t, "cleanup.ready")
			f.locked(t, true)
			f.release(t, "cleanup")
			f.wait(t, "work.ready")
			f.locked(t, true)
			if tc.signal != 0 {
				if err := r.cmd.Process.Signal(tc.signal); err != nil {
					t.Fatal(err)
				}
			}
			if tc.signal == syscall.SIGKILL {
				r.result(t, tc.want)
				f.locked(t, false)
				return
			}
			if tc.signal != 0 {
				f.locked(t, true)
			}
			f.release(t, "work")
			r.result(t, tc.want)
			// The auxiliary process still lives; it must never extend the host lease.
			b, err := os.ReadFile(filepath.Join(f.root, "descendant.pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(string(b))
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("fixture descendant did not survive: %v", err)
			}
			f.locked(t, false)
		})
	}
}

func TestVMTestGroupInterrupt(t *testing.T) {
	f := newLockFixture(t)
	r := f.start(t, 0)
	f.wait(t, "cleanup.ready")
	f.release(t, "cleanup")
	f.wait(t, "work.ready")
	if err := syscall.Kill(-r.cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	r.result(t, 130)
	f.locked(t, false)
}

func TestVMTestRetentionLease(t *testing.T) {
	f := newLockFixture(t)
	rm, err := exec.LookPath("rm")
	if err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "KATL_VMTEST_KEEP=never", "KATL_LOCK_RM="+rm, "PATH="+f.root+":"+os.Getenv("PATH"))
	r := f.start(t, 0)
	f.wait(t, "cleanup.ready")
	f.release(t, "cleanup")
	f.wait(t, "work.ready")
	f.release(t, "work")
	f.wait(t, "retention.ready")
	f.locked(t, true)

	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	f.locked(t, true)
	f.release(t, "retention")
	r.result(t, 143)
	f.locked(t, false)
	if _, err := os.Stat(filepath.Join(f.root, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory was not removed: %v", err)
	}
}

func TestVMTestCancelledWaiter(t *testing.T) {
	f := newLockFixture(t)
	lock, err := os.OpenFile(filepath.Join(f.root, "host.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	r := f.start(t, 0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(r.log)
		if strings.Contains(string(b), "vmtest host lock: waiting") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter did not reach lease: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-r.cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if err == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not cancel")
	}
	for _, name := range []string{"cleanup.ready", "work.ready"} {
		if _, err := os.Stat(filepath.Join(f.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled waiter entered %s", name)
		}
	}
	f.locked(t, true)
}
