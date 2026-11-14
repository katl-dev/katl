// This probe runs as ExecStartPost under the service's actual execution policy.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func main() {
	if err := probe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("service security probe passed")
}

func probe() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: service-security ROLE WRITABLE_DIRECTORY")
	}
	if os.Args[1] == "native" {
		privileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
		if err != nil {
			return err
		}
		mapping, err := unix.Mmap(-1, 0, os.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE|unix.PROT_EXEC, unix.MAP_PRIVATE|unix.MAP_ANON)
		if err == nil {
			unix.Munmap(mapping)
		}
		fmt.Printf("uid=%d noNewPrivileges=%d executableMemory=%t\n", os.Geteuid(), privileges, err == nil)
		return nil
	}
	if os.Args[1] == "proxy" && os.Geteuid() == 0 {
		return fmt.Errorf("API proxy runs as root")
	}
	privileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || privileges != 1 {
		return fmt.Errorf("service can gain privileges: %d, %v", privileges, err)
	}

	file, err := os.CreateTemp(os.Args[2], ".security-probe-")
	if err != nil {
		return fmt.Errorf("service cannot write its runtime directory: %w", err)
	}
	file.Close()
	defer os.Remove(file.Name())

	outside := "/var/lib/katl/test-artifacts/security-unexpected"
	err = os.WriteFile(outside, []byte("unexpected write"), 0o600)
	if err == nil {
		os.Remove(outside)
		return fmt.Errorf("service can write outside its runtime directory")
	}
	if !errors.Is(err, unix.EROFS) && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("unexpected filesystem denial: %w", err)
	}

	if os.Args[1] == "proxy" {
		if _, err := os.ReadFile("/var/lib/katl/test-artifacts/security-secret"); !errors.Is(err, unix.EACCES) {
			return fmt.Errorf("API proxy can read root credentials, or fixture is missing: %v", err)
		}
	}

	mapping, err := unix.Mmap(-1, 0, os.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE|unix.PROT_EXEC, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err == nil {
		unix.Munmap(mapping)
		return fmt.Errorf("service can allocate writable executable memory")
	}
	if !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) {
		return fmt.Errorf("unexpected executable memory denial: %w", err)
	}

	if err := unix.Unshare(unix.CLONE_NEWUSER); !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("service can create a user namespace: %v", err)
	}
	if err := unix.Mount("none", filepath.Dir(file.Name()), "tmpfs", 0, "size=4096"); !errors.Is(err, unix.EPERM) {
		if err == nil {
			unix.Unmount(filepath.Dir(file.Name()), 0)
		}
		return fmt.Errorf("service can install a mount: %v", err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err == nil {
		unix.Close(fd)
		return fmt.Errorf("service can open raw packet sockets")
	}
	if !errors.Is(err, unix.EAFNOSUPPORT) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) {
		return fmt.Errorf("unexpected packet socket denial: %w", err)
	}
	return nil
}
