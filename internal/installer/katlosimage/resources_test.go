package katlosimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverFailureReleasesResources(t *testing.T) {
	for _, phase := range []string{"download", "hash", "mount", "applied mount error", "validation", "cancellation"} {
		t.Run(phase, func(t *testing.T) {
			work := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := expectedImage()
			expected.LocalRef, expected.URL, expected.SHA256, expected.SizeBytes = "", "https://example.invalid/image", "", 0
			commands := &resourceMountRunner{populate: func(root string) {
				writeImagePayloadAt(t, root, nil)
				if phase == "validation" {
					if err := os.WriteFile(filepath.Join(root, "components/runtime/root.squashfs"), []byte("corrupt"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "cancellation" {
					cancel()
				}
			}}
			client := &fixtureHTTPClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("image bytes"))}}
			switch phase {
			case "download":
				client.err = errors.New("connection lost")
			case "hash":
				expected.SHA256 = strings.Repeat("f", 64)
			case "mount":
				commands.mountErr = errors.New("mount failed")
			case "applied mount error":
				commands.mountErr = errors.New("mount reported failure after acquisition")
				commands.appliedError = true
			}

			_, err := (RemoteResolver{WorkDir: work, Commands: commands, Client: client}).ResolveKatlosImage(ctx, expected)
			if err == nil {
				t.Fatal("failed resolution succeeded")
			}
			entries, readErr := os.ReadDir(filepath.Join(work, "acquisitions"))
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("failed resolution retained acquisitions: %v, %v", entries, readErr)
			}
			if (phase == "validation" || phase == "cancellation" || phase == "applied mount error") && commands.unmounts != 1 {
				t.Fatalf("mounted failure did not release mount: %d", commands.unmounts)
			}
		})
	}
}

func TestPayloadCloseRequiresAbsence(t *testing.T) {
	for _, test := range []struct {
		name        string
		probeErr    error
		keepMounted bool
	}{
		{name: "probe failed", probeErr: resourceExit(1)},
		{name: "probe unavailable", probeErr: errors.New("mountpoint unavailable")},
		{name: "unmount left mounted", keepMounted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := &resourceMountRunner{populate: func(root string) { writeImagePayloadAt(t, root, nil) }}
			expected := expectedImage()
			expected.LocalRef, expected.URL, expected.SHA256, expected.SizeBytes = "", "https://example.invalid/image", "", 0
			client := &fixtureHTTPClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("retained image"))}}
			payload, err := (RemoteResolver{WorkDir: t.TempDir(), Commands: commands, Client: client}).ResolveKatlosImage(context.Background(), expected)
			if err != nil {
				t.Fatal(err)
			}
			commands.probeErr, commands.keepMounted = test.probeErr, test.keepMounted
			if err := payload.Close(); err == nil {
				t.Fatal("ambiguous mount absence was accepted")
			}
			if data, err := os.ReadFile(payload.ImagePath); err != nil || string(data) != "retained image" {
				t.Fatalf("uncertain mount lost backing data: %q, %v", data, err)
			}
			commands.probeErr, commands.keepMounted = nil, false
			if err := payload.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolverPreservesMountCleanupFailure(t *testing.T) {
	work := t.TempDir()
	reported := errors.New("mount reported failure after acquisition")
	probe := resourceExit(1)
	commands := &resourceMountRunner{mountErr: reported, appliedError: true, probeErr: probe, populate: func(root string) { writeImagePayloadAt(t, root, nil) }}
	expected := expectedImage()
	expected.LocalRef, expected.URL, expected.SHA256, expected.SizeBytes = "", "https://example.invalid/image", "", 0
	client := &fixtureHTTPClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("retained image"))}}
	_, err := (RemoteResolver{WorkDir: work, Commands: commands, Client: client}).ResolveKatlosImage(context.Background(), expected)
	if !errors.Is(err, reported) || !errors.Is(err, probe) {
		t.Fatalf("resolution lost original or cleanup error: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(work, "acquisitions/*/downloads/*.squashfs"))
	if err != nil || len(files) != 1 {
		t.Fatalf("unproven mount absence lost backing image: %v, %v", files, err)
	}
}

func TestPayloadCloseRetainsFailedMount(t *testing.T) {
	work := t.TempDir()
	busy := errors.New("mount is busy")
	commands := &resourceMountRunner{unmountErr: busy, populate: func(root string) { writeImagePayloadAt(t, root, nil) }}
	expected := expectedImage()
	expected.LocalRef, expected.URL, expected.SHA256, expected.SizeBytes = "", "https://example.invalid/image", "", 0
	image := []byte("owned downloaded image")
	client := &fixtureHTTPClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(image))}}
	payload, err := (RemoteResolver{WorkDir: work, Commands: commands, Client: client}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	copy := payload
	if err := copy.Close(); !errors.Is(err, busy) {
		t.Fatalf("close error = %v", err)
	}
	if data, err := os.ReadFile(payload.ImagePath); err != nil || !bytes.Equal(data, image) {
		t.Fatalf("failed unmount lost backing image: %q, %v", data, err)
	}
	if _, err := os.ReadFile(filepath.Join(payload.Root, "katlos/image.json")); err != nil {
		t.Fatalf("failed unmount lost mounted data: %v", err)
	}
	commands.unmountErr = nil
	if err := payload.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload.ImagePath); !os.IsNotExist(err) {
		t.Fatalf("released download remains: %v", err)
	}
	if commands.unmounts != 2 {
		t.Fatalf("copied payload released mount %d times", commands.unmounts)
	}
}

func TestPayloadCloseReleasesDownload(t *testing.T) {
	commands := &resourceMountRunner{populate: func(root string) { writeImagePayloadAt(t, root, nil) }}
	expected := expectedImage()
	expected.LocalRef, expected.URL, expected.SHA256, expected.SizeBytes = "", "https://example.invalid/image", "", 0
	client := &fixtureHTTPClient{response: &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("owned image"))}}
	payload, err := (RemoteResolver{WorkDir: t.TempDir(), Commands: commands, Client: client}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(payload.ImagePath); err != nil || string(data) != "owned image" {
		t.Fatalf("active image not readable: %q, %v", data, err)
	}
	if err := payload.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload.ImagePath); !os.IsNotExist(err) {
		t.Fatalf("closed payload retained owned download: %v", err)
	}
}

func TestLocalFailurePreservesBorrowedImage(t *testing.T) {
	media := t.TempDir()
	image := []byte("borrowed media")
	path := filepath.Join(media, "image.squashfs")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := &resourceMountRunner{populate: func(root string) { writeImagePayloadAt(t, root, func(index *Index) { index.Kind = "invalid" }) }}
	work := t.TempDir()
	expected := expectedImage()
	expected.LocalRef, expected.SHA256, expected.SizeBytes = "image.squashfs", "", 0
	if _, err := (LocalResolver{MediaRoot: media, WorkDir: work, Commands: commands}).ResolveKatlosImage(context.Background(), expected); err == nil {
		t.Fatal("invalid image was accepted")
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, image) {
		t.Fatalf("borrowed input was changed: %q, %v", data, err)
	}
	if commands.unmounts != 1 {
		t.Fatalf("failed local validation retained mount: %d", commands.unmounts)
	}
}

type resourceMountRunner struct {
	populate             func(string)
	mountErr, unmountErr error
	unmounts             int
	mounted              map[string]bool
	probeErr             error
	appliedError         bool
	keepMounted          bool
}

func (r *resourceMountRunner) Run(ctx context.Context, name string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch name {
	case "mountpoint":
		if r.probeErr != nil {
			return r.probeErr
		}
		if r.mounted[args[len(args)-1]] {
			return nil
		}
		return resourceExit(32)
	case "mount":
		if r.mountErr != nil && !r.appliedError {
			return r.mountErr
		}
		if r.mounted == nil {
			r.mounted = map[string]bool{}
		}
		r.mounted[args[len(args)-1]] = true
		r.populate(args[len(args)-1])
		return r.mountErr
	case "umount":
		r.unmounts++
		if r.unmountErr == nil && !r.keepMounted {
			delete(r.mounted, args[len(args)-1])
		}
		return r.unmountErr
	}
	return nil
}

type resourceExit int

func (code resourceExit) Error() string { return fmt.Sprintf("mountpoint exit %d", code) }
func (code resourceExit) ExitCode() int { return int(code) }
