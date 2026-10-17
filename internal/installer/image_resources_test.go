package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/imagemount"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestInstallImageRetryReleasesAttempt(t *testing.T) {
	state := t.TempDir()
	work := filepath.Join(state, "images")
	image := []byte("one image fits")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		data := image
		if request.URL.Path == "/2" {
			data = []byte("new image fits")
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	commands := &installImageCommands{}
	available := func(string) (uint64, error) {
		var retained uint64
		err := filepath.WalkDir(work, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.Contains(path, string(filepath.Separator)+"downloads"+string(filepath.Separator)) {
				return err
			}
			info, err := entry.Info()
			if err == nil {
				retained += uint64(info.Size())
			}
			return err
		})
		return uint64(len(image)) - retained, err
	}
	rejected := errors.New("selected target does not satisfy install policy")
	for attempt := 0; attempt < 3; attempt++ {
		path := writeManifest(t)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := manifest.Decode(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		imageBytes := image
		if attempt == 2 {
			imageBytes = []byte("new image fits")
		}
		sum := sha256.Sum256(imageBytes)
		decoded.KatlosImage.URL, decoded.KatlosImage.SHA256, decoded.KatlosImage.SizeBytes = server.URL+"/"+fmt.Sprint(attempt), hex.EncodeToString(sum[:]), uint64(len(imageBytes))
		data, err = json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		install := &Context{ManifestPath: path, StateDir: state, Store: NewFileStateStore(state), Commands: commands,
			KatlosResolver: katlosimage.RemoteResolver{WorkDir: work, Client: server.Client(), Commands: commands, AvailableBytes: available}}
		gate := &imagePolicyStep{}
		if attempt < 2 {
			gate.err = rejected
		}
		err = NewRunner(Plan{loadManifestStep{}, verifyKatlosImageStep{}, gate}, install).Run(context.Background())
		if attempt < 2 {
			if !errors.Is(err, rejected) {
				t.Fatalf("attempt %d lost policy rejection: %v", attempt, err)
			}
			record, readErr := installstatus.ReadFile(filepath.Join(state, "status.json"))
			if readErr != nil || record.State != installstatus.StateFailedBeforeMutation || !strings.Contains(record.LastError, rejected.Error()) || record.CurrentStep != string(PlanInstall) {
				t.Fatalf("rejection evidence = %+v, %v", record, readErr)
			}
		} else if err != nil || !gate.ran {
			t.Fatalf("corrected attempt did not progress: %v", err)
		}
		if _, err := os.Stat(install.KatlosImage.ImagePath); !os.IsNotExist(err) {
			t.Fatalf("attempt %d retained its download: %v", attempt, err)
		}
		if len(commands.NoopCommandRunner.Calls) != 0 {
			t.Fatalf("attempt made disk commands: %v", commands.NoopCommandRunner.Calls)
		}
	}
}

type imagePolicyStep struct {
	err    error
	ran    bool
	cancel context.CancelFunc
}

func (*imagePolicyStep) ID() StepID { return PlanInstall }
func (s *imagePolicyStep) Run(_ context.Context, install *Context) error {
	s.ran = true
	if s.cancel != nil {
		s.cancel()
	}
	return s.err
}

type installImageCommands struct {
	NoopCommandRunner
	mounted         map[string]bool
	unmountErr      error
	unmounts        int
	failUnmountOnce bool
}

func (c *installImageCommands) Run(ctx context.Context, name string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "mountpoint" {
		if c.mounted[args[len(args)-1]] {
			return nil
		}
		return installMountExit(32)
	}
	if name != "mount" && name != "umount" {
		return c.NoopCommandRunner.Run(ctx, name, args...)
	}
	if name == "umount" {
		c.unmounts++
		if c.unmountErr != nil {
			err := c.unmountErr
			if c.failUnmountOnce {
				c.unmountErr = nil
			}
			return err
		}
		delete(c.mounted, args[len(args)-1])
		return ctx.Err()
	}
	root := args[len(args)-1]
	if c.mounted == nil {
		c.mounted = map[string]bool{}
	}
	c.mounted[root] = true
	index := katlosimage.Index{APIVersion: katlosimage.APIVersion, Kind: katlosimage.Kind, ImageRole: katlosimage.RoleInstall, Format: "squashfs", Version: "2026.06.04", Architecture: "x86_64", RuntimeInterface: "katl-runtime-1"}
	for _, role := range []string{katlosimage.ComponentRuntimeRoot, katlosimage.ComponentRuntimeUKI} {
		content := []byte(role)
		sum := sha256.Sum256(content)
		component := katlosimage.Component{Name: role, Role: role, Path: "components/" + role, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), Version: index.Version, Architecture: index.Architecture}
		component.Compatibility.RuntimeInterface = index.RuntimeInterface
		component.InstallTarget.Kind = "root-slot"
		if role == katlosimage.ComponentRuntimeUKI {
			component.InstallTarget.Kind = "esp-or-xbootldr"
			component.Compatibility.RuntimeRoot.ArtifactSHA256 = index.Components[0].SHA256
			component.Compatibility.KernelCommandLine = []string{"rootfstype=squashfs", "ro"}
		}
		path := filepath.Join(root, component.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
		index.Components = append(index.Components, component)
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "katlos"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "katlos/image.json"), data, 0o600)
}

func TestInstallImageCleanupEvidence(t *testing.T) {
	for _, mode := range []string{"policy rejection", "before reboot", "cancellation", "retained module consumer"} {
		t.Run(mode, func(t *testing.T) {
			busy := errors.New("image is busy")
			commands := &installImageCommands{}
			if mode == "policy rejection" || mode == "before reboot" {
				commands.unmountErr, commands.failUnmountOnce = busy, true
			}
			image := []byte("image")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = w.Write(image) }))
			defer server.Close()
			state := t.TempDir()
			path := writeManifest(t)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := manifest.Decode(strings.NewReader(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(image)
			decoded.KatlosImage.URL, decoded.KatlosImage.SHA256, decoded.KatlosImage.SizeBytes = server.URL, hex.EncodeToString(sum[:]), uint64(len(image))
			data, err = json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			install := &Context{ManifestPath: path, Store: NewFileStateStore(state), Commands: commands, KatlosResolver: katlosimage.RemoteResolver{WorkDir: filepath.Join(state, "images"), Commands: commands}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("install policy refused the image")
			gate := &imagePolicyStep{err: cause}
			var last Step = gate
			if mode == "before reboot" {
				last = rebootStep{}
			}
			if mode == "cancellation" {
				cause, gate.err, gate.cancel = context.Canceled, context.Canceled, cancel
			}
			if mode == "retained module consumer" {
				last = retainedConsumerStep{cause: cause, busy: busy}
			}
			err = NewRunner(Plan{loadManifestStep{}, verifyKatlosImageStep{}, last}, install).Run(ctx)
			if mode != "before reboot" && !errors.Is(err, cause) || mode != "cancellation" && !errors.Is(err, busy) {
				t.Fatalf("attempt lost original or cleanup cause: %v", err)
			}
			record, readErr := installstatus.ReadFile(filepath.Join(state, "status.json"))
			wantDiagnostic := busy.Error()
			if mode == "cancellation" {
				wantDiagnostic = context.Canceled.Error()
			}
			if readErr != nil || record.CurrentStep != string(last.ID()) || record.State != installstatus.StateFailedBeforeMutation || record.DestructiveMutation || record.RequestDigest == "" || !strings.Contains(record.LastError, wantDiagnostic) {
				t.Fatalf("durable failure omitted cleanup: %+v, %v", record, readErr)
			}
			if mode == "policy rejection" && !strings.Contains(record.LastError, cause.Error()) {
				t.Fatalf("cleanup obscured the original rejection: %s", record.LastError)
			}
			wantUnmounts := 1
			if mode == "retained module consumer" {
				wantUnmounts = 0
			}
			if commands.unmounts != wantUnmounts {
				t.Fatalf("cleanup retried before returning failure: %d", commands.unmounts)
			}
			_, statErr := os.Stat(install.KatlosImage.ImagePath)
			if mode == "cancellation" && !os.IsNotExist(statErr) || mode != "cancellation" && statErr != nil {
				t.Fatalf("cleanup did not preserve its resource outcome: %v", statErr)
			}
			if len(commands.NoopCommandRunner.Calls) != 0 {
				t.Fatalf("failed cleanup requested lifecycle commands: %v", commands.NoopCommandRunner.Calls)
			}
		})
	}
}

// A failed module constructor has no usable result, so its native release error
// must carry ownership through the ordinary step error to the enclosing image.
type retainedConsumerStep struct{ cause, busy error }

func (retainedConsumerStep) ID() StepID { return PlanInstall }
func (s retainedConsumerStep) Run(_ context.Context, _ *Context) error {
	return errors.Join(s.cause, imagemount.DissectUnmount(busyModuleMount{s.busy}, "/owned/module-mount"))
}

type busyModuleMount struct{ err error }

func (c busyModuleMount) Run(_ context.Context, name string, _ ...string) error {
	if name == "mountpoint" {
		return nil
	}
	return c.err
}

type installMountExit int

func (code installMountExit) Error() string { return fmt.Sprintf("mountpoint exit %d", code) }
func (code installMountExit) ExitCode() int { return int(code) }
