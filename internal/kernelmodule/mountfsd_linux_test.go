package kernelmodule

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestServiceImage(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "nested", "hidden.ko")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("module marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	reply := `{"parameters":{"partitions":[{"designator":"root","writable":false,"mountFileDescriptor":0}]}}`
	address := serveImage(t, func(connection *net.UnixConn) {
		// Split the Varlink frame, including delivery of its descriptor.
		if _, _, err := connection.WriteMsgUnix([]byte(reply[:25]), unix.UnixRights(int(file.Fd())), nil); err != nil {
			t.Error(err)
			return
		}
		if _, err := connection.Write(append([]byte(reply[25:]), 0)); err != nil {
			t.Error(err)
		}
	})
	view, err := openServiceImage(context.Background(), address, marker)
	if err != nil {
		t.Fatal(err)
	}
	defer view.descriptor.Close()

	var names []string
	if err := filepath.WalkDir(view.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "hidden.ko" {
		t.Fatalf("full image traversal found %v", names)
	}
	_, err = inspectSelections(context.Background(), Target{Release: "6.12.1", RuntimeSHA256: strings.Repeat("a", 64)}, []mountedBundle{{Name: "userspace", Roots: []string{view.root}}})
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("userspace image hid an undeclared module: %v", err)
	}
	output, err := exec.Command("cat", filepath.Join(view.root, "nested/hidden.ko")).CombinedOutput()
	if err != nil || string(output) != "module marker" {
		t.Fatalf("child image access = %q, %v", output, err)
	}
	flags, err := unix.FcntlInt(view.descriptor.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("image descriptor lacks CLOEXEC: flags=%d err=%v", flags, err)
	}
}

func TestServiceImageReplyLimit(t *testing.T) {
	root := t.TempDir()
	file, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	address := serveImage(t, func(connection *net.UnixConn) {
		_, _, _ = connection.WriteMsgUnix([]byte(" "), unix.UnixRights(int(file.Fd())), nil)
		_, _ = io.Copy(connection, strings.NewReader(strings.Repeat(" ", 1<<20)+"{}\x00"))
	})
	before := descriptorsFor(t, root)
	if view, err := openServiceImage(context.Background(), address, root); err == nil {
		_ = view.descriptor.Close()
		t.Fatal("accepted an oversized reply")
	}
	if after := descriptorsFor(t, root); after != before {
		t.Fatalf("oversized reply retained descriptors: before=%d after=%d", before, after)
	}
}

func TestServiceImageRejectsInvalidReplies(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply string
	}{
		{"writable", `{"parameters":{"partitions":[{"designator":"root","writable":true,"mountFileDescriptor":0}]}}`},
		{"unspecified access", `{"parameters":{"partitions":[{"designator":"root","mountFileDescriptor":0}]}}`},
		{"missing descriptor", `{"parameters":{"partitions":[{"designator":"root","writable":false}]}}`},
		{"negative descriptor", `{"parameters":{"partitions":[{"designator":"root","writable":false,"mountFileDescriptor":-1}]}}`},
		{"unknown descriptor", `{"parameters":{"partitions":[{"designator":"root","writable":false,"mountFileDescriptor":1}]}}`},
		{"usr only", `{"parameters":{"partitions":[{"designator":"usr","writable":false,"mountFileDescriptor":0}]}}`},
		{"split filesystem", `{"parameters":{"partitions":[{"designator":"root","writable":false,"mountFileDescriptor":0},{"designator":"usr","writable":false,"mountFileDescriptor":0}]}}`},
		{"service policy", `{"error":"io.systemd.MountFileSystem.DeniedByImagePolicy"}`},
		{"malformed", `{`},
		{"unterminated", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			file, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			address := serveImage(t, func(connection *net.UnixConn) {
				reply := []byte(test.reply)
				if test.name != "unterminated" {
					reply = append(reply, 0)
				} else {
					reply = []byte(`{"parameters":`)
				}
				if _, _, err := connection.WriteMsgUnix(reply, unix.UnixRights(int(file.Fd())), nil); err != nil {
					t.Error(err)
				}
			})
			before := descriptorsFor(t, root)
			if view, err := openServiceImage(context.Background(), address, root); err == nil {
				_ = view.descriptor.Close()
				t.Fatal("accepted an invalid service reply")
			}
			if after := descriptorsFor(t, root); after != before {
				t.Fatalf("failed request retained image descriptors: before=%d after=%d", before, after)
			}
		})
	}
}

func TestServiceImageCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := serveImage(t, func(connection *net.UnixConn) {
		cancel()
		_, _ = io.Copy(io.Discard, connection)
	})
	if _, err := openServiceImage(ctx, address, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
}

func serveImage(t *testing.T, respond func(*net.UnixConn)) string {
	t.Helper()
	// Keep Unix socket paths below the platform's fixed length limit.
	directory, err := os.MkdirTemp("", "katl-mountfsd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	address := filepath.Join(directory, "socket")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("image service did not release connection")
		}
	})
	go func() {
		defer close(done)
		connection, err := listener.AcceptUnix()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		message, control := make([]byte, 4096), make([]byte, unix.CmsgSpace(4))
		n, controlCount, _, _, err := connection.ReadMsgUnix(message, control)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Contains(message[:n], []byte(`"readOnly":true`)) || !bytes.Contains(message[:n], []byte(`"growFileSystems":false`)) {
			t.Errorf("image request permits mutation: %s", message[:n])
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlCount])
		if err != nil {
			t.Error(err)
			return
		}
		for _, message := range messages {
			fds, err := unix.ParseUnixRights(&message)
			if err != nil {
				t.Error(err)
				return
			}
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
		}
		respond(connection)
	}()
	return address
}

func descriptorsFor(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		target, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%s", entry.Name()))
		if strings.TrimSuffix(target, " (deleted)") == path {
			count++
		}
	}
	return count
}
