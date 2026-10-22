package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/resourcetest"
)

type source struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
}

type buildRecord struct {
	Identity string            `json:"identity"`
	Sources  map[string]source `json:"sources"`
	Packages map[string]string `json:"packages"`
}

var stableVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "katl-runtime-rpms:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("katl-runtime-rpms", flag.ContinueOnError)
	repo := flags.String("repo", ".", "repository containing packages/")
	output := flags.String("output", "_build/mkosi/runtime-rpms", "RPM and mkosi configuration output")
	cache := flags.String("cache", "_build/mkosi/package-cache/runtime-rpms", "verified RPM cache")
	fedora := flags.String("fedora-release", "", "Fedora release used by the builder")
	inventory := flags.String("inventory", "", "verify an installed RPM inventory instead of building")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !regexp.MustCompile(`^[0-9]+$`).MatchString(*fedora) {
		return fmt.Errorf("--fedora-release is required; no positional arguments are accepted")
	}
	var err error
	for _, path := range []*string{repo, output, cache} {
		*path, err = filepath.Abs(*path)
		if err != nil {
			return err
		}
	}
	sources := make(map[string]source)
	for _, name := range []string{"containerd", "crun"} {
		s, err := readSource(filepath.Join(*repo, "packages", name, "source.json"), name)
		if err != nil {
			return err
		}
		sources[name] = s
	}
	if *inventory != "" {
		return verifyInventory(*inventory, sources, *fedora)
	}
	for _, path := range []string{*cache, *output} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(filepath.Join(*cache, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Standard and LTS builds can share this cache; publish complete RPM sets under one lock.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	identity := sha256.New()
	fmt.Fprintln(identity, "fedora-release="+*fedora)
	builderPackages, err := exec.Command("rpm", "-qa", "--qf", "%{NEVRA}\n").Output()
	if err != nil {
		return fmt.Errorf("inventory builder: %w", err)
	}
	// A changed host library or compiler must not reuse an RPM from the old builder.
	lines := strings.Split(string(builderPackages), "\n")
	slices.Sort(lines)
	fmt.Fprintln(identity, strings.Join(lines, "\n"))
	for _, name := range []string{"containerd", "crun"} {
		for _, file := range []string{"source.json", name + ".spec"} {
			data, err := os.ReadFile(filepath.Join(*repo, "packages", name, file))
			if err != nil {
				return err
			}
			identity.Write(data)
		}
	}
	toolSources, err := filepath.Glob(filepath.Join(*repo, "cmd", "katl-runtime-rpms", "*.go"))
	if err != nil {
		return err
	}
	for _, path := range toolSources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		identity.Write(data)
	}
	key := hex.EncodeToString(identity.Sum(nil))
	set := filepath.Join(*cache, key)
	record := buildRecord{Identity: key, Sources: sources, Packages: map[string]string{}}
	if !validCache(set, key) {
		work, err := os.MkdirTemp(*cache, ".build-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		for _, name := range []string{"containerd", "crun"} {
			if err := buildRPM(*repo, work, name, sources[name], *fedora); err != nil {
				return fmt.Errorf("build %s: %w", name, err)
			}
		}
		rpms, err := filepath.Glob(filepath.Join(work, "RPMS", "x86_64", "*.rpm"))
		if err != nil {
			return err
		}
		if len(rpms) != 2 {
			return fmt.Errorf("expected two runtime RPMs, found %d", len(rpms))
		}
		if err := os.MkdirAll(set, 0o755); err != nil {
			return err
		}
		for _, path := range rpms {
			digest, err := fileDigest(path)
			if err != nil {
				return err
			}
			name := filepath.Base(path)
			record.Packages[name] = digest
			if err := copyFile(path, filepath.Join(set, name)); err != nil {
				return err
			}
		}
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(set, "build.json"), append(data, '\n'), 0o644); err != nil {
			return err
		}
	}

	data, err := os.ReadFile(filepath.Join(set, "build.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	destination := filepath.Join(*output, key)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for name := range record.Packages {
		if err := copyFile(filepath.Join(set, name), filepath.Join(destination, name)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(*output, "build.json"), data, 0o644); err != nil {
		return err
	}
	config := "[Content]\nPackageDirectories=" + destination + "\nPackages=\n"
	for _, name := range []string{"containerd", "crun"} {
		// Exact NEVRAs prevent the distribution repository from supplying a different runtime.
		config += fmt.Sprintf("        %s-%s-1.katl.fc%s.x86_64\n", name, strings.TrimPrefix(sources[name].Version, "v"), *fedora)
	}
	return os.WriteFile(filepath.Join(*output, "mkosi.conf"), []byte(config), 0o644)
}

func verifyInventory(path string, sources map[string]source, fedora string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	packages, err := resourcetest.ParseRPMPackages(f)
	if err != nil {
		return err
	}
	for _, name := range []string{"containerd", "crun"} {
		want := fmt.Sprintf("0:%s-1.katl.fc%s.x86_64", strings.TrimPrefix(sources[name].Version, "v"), fedora)
		count := 0
		for _, pkg := range packages {
			if pkg.Name == name {
				count++
				if pkg.NEVRA != want {
					return fmt.Errorf("runtime requires %s %s; installed %s", name, want, pkg.NEVRA)
				}
			}
		}
		if count != 1 {
			return fmt.Errorf("runtime requires exactly one %s package; found %d", name, count)
		}
	}
	return nil
}

func readSource(path, name string) (source, error) {
	var s source
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if !stableVersion.MatchString(s.Version) {
		return s, fmt.Errorf("%s requires a stable upstream version", name)
	}
	if name == "containerd" {
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(s.Revision) || !stableVersion.MatchString(s.GoVersion) {
			return s, fmt.Errorf("containerd requires a pinned commit and Go toolchain")
		}
	} else if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.SHA256) {
		return s, fmt.Errorf("crun requires a source SHA-256")
	}
	return s, nil
}

func buildRPM(repo, work, name string, s source, fedora string) error {
	sources := filepath.Join(work, "SOURCES")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		return err
	}
	version := strings.TrimPrefix(s.Version, "v")
	if err := copyFile(filepath.Join(repo, "packages", name, "source.json"), filepath.Join(sources, name+"-source.json")); err != nil {
		return err
	}
	if name == "containerd" {
		directory := filepath.Join(work, "containerd-git")
		if err := command(work, "git", "init", "--quiet", directory); err != nil {
			return err
		}
		if err := command(directory, "git", "fetch", "--depth=1", "https://github.com/containerd/containerd.git", "refs/tags/"+s.Version); err != nil {
			return err
		}
		commit, err := exec.Command("git", "-C", directory, "rev-parse", "FETCH_HEAD^{commit}").Output()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(commit)) != s.Revision {
			return fmt.Errorf("containerd tag %s does not match pinned commit %s", s.Version, s.Revision)
		}
		if err := command(directory, "git", "archive", "--format=tar.gz", "--prefix=containerd-"+version+"/", "--output="+filepath.Join(sources, "containerd-"+version+".tar.gz"), s.Revision); err != nil {
			return err
		}
	} else {
		archive := "crun-" + version + ".tar.zst"
		if err := download("https://github.com/containers/crun/releases/download/"+s.Version+"/"+archive, filepath.Join(sources, archive), s.SHA256); err != nil {
			return err
		}
	}
	args := []string{"-bb", "--define", "_topdir " + work,
		"--define", "_smp_mflags -j2", "--define", "debug_package %{nil}",
		"--define", "dist .fc" + fedora, "--define", "upstream_version " + version}
	if name == "containerd" {
		args = append(args, "--define", "upstream_revision "+s.Revision, "--define", "go_toolchain go"+s.GoVersion)
	}
	args = append(args, filepath.Join(repo, "packages", name, name+".spec"))
	return command(repo, "rpmbuild", args...)
}

func command(directory, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = directory, os.Stdout, os.Stderr
	return cmd.Run()
}

func validCache(directory, identity string) bool {
	data, err := os.ReadFile(filepath.Join(directory, "build.json"))
	if err != nil {
		return false
	}
	var record buildRecord
	if json.Unmarshal(data, &record) != nil || record.Identity != identity || len(record.Packages) != 2 {
		return false
	}
	for name, digest := range record.Packages {
		if name != filepath.Base(name) || !strings.HasSuffix(name, ".rpm") {
			return false
		}
		actual, err := fileDigest(filepath.Join(directory, name))
		if err != nil || actual != digest {
			return false
		}
	}
	return true
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func download(url, destination, digest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, response.Status)
	}
	f, err := os.Create(destination)
	if err != nil {
		return err
	}
	size, copyErr := io.Copy(f, io.LimitReader(response.Body, (64<<20)+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	actual, err := fileDigest(destination)
	if err != nil {
		return err
	}
	if size > 64<<20 || actual != digest {
		return fmt.Errorf("source SHA-256 verification failed for %s", url)
	}
	return nil
}
