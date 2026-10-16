package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/kernelrelease"
)

func candidate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-kernel-release candidate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flavour := flags.String("flavour", "standard", "standard or lts")
	fedora := flags.String("fedora-release", "", "Fedora release")
	architecture := flags.String("architecture", "x86_64", "RPM architecture")
	release := flags.String("kernel-release", "", "optional exact kernel release; empty discovers latest")
	output := flags.String("output", "_build/kernel-candidate", "private candidate output directory")
	repository := flags.String("repository", "ghcr.io/katl-dev/katl/kernel-inputs", "retained OCI repository")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fedora == "" {
		return fmt.Errorf("candidate requires --fedora-release")
	}
	if !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(*fedora) {
		return fmt.Errorf("Fedora release must be a positive integer")
	}
	if *release != "" && !regexp.MustCompile(`^[0-9][a-zA-Z0-9._+-]*$`).MatchString(*release) {
		return fmt.Errorf("kernel release must be a safe RPM version and release")
	}
	absoluteOutput, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	*output = absoluteOutput
	names, err := kernelrelease.Names(*flavour)
	if err != nil {
		return err
	}
	if *architecture != "x86_64" {
		return fmt.Errorf("kernel inputs support x86_64")
	}
	// Candidate resolution is the only place that queries moving repositories.
	// Every subsequent build consumes the resulting retained byte identity.
	query := []string{"--quiet", "--releasever=" + *fedora, "--forcearch=" + *architecture}
	key := "/etc/pki/rpm-gpg/RPM-GPG-KEY-fedora-" + *fedora + "-primary"
	if *flavour == "lts" {
		base := "https://download.copr.fedorainfracloud.org/results/kwizart/kernel-longterm-6.18/"
		query = append(query, "--repofrompath=katl-kernel-longterm,"+base+"fedora-"+*fedora+"-"+*architecture+"/", "--repo=katl-kernel-longterm")
		key = base + "pubkey.gpg"
	}
	selector := names[0]
	if *release != "" {
		selector += "-" + *release
	}
	rows, err := queryRPMs(ctx, append(query, "repoquery", "--latest-limit=1", "--arch="+*architecture, "--queryformat=%{name}\t%{full_nevra}\t%{version}-%{release}.%{arch}\t%{location}\n", selector), stderr)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return fmt.Errorf("kernel discovery requires one %s candidate, got %d", selector, len(rows))
	}
	selected := strings.Split(rows[0], "\t")
	if len(selected) != 4 {
		return fmt.Errorf("invalid kernel discovery result")
	}
	set := kernelrelease.Set{SchemaVersion: 1, FedoraRelease: *fedora, Architecture: *architecture, Flavour: *flavour, KernelRelease: selected[2]}
	if !regexp.MustCompile(`^[0-9][a-zA-Z0-9._+-]*$`).MatchString(set.KernelRelease) || !strings.HasSuffix(set.KernelRelease, ".fc"+*fedora+"."+*architecture) {
		return fmt.Errorf("discovered kernel does not match the selected Fedora release and architecture")
	}
	if *release != "" && *release != set.KernelRelease {
		return fmt.Errorf("resolved kernel does not match the requested release")
	}
	// Keep incomplete captures private; only the final lock names usable bytes.
	if err := os.MkdirAll(*output, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(*output, ".capture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	rpms := filepath.Join(staging, "rpms")
	if err := os.Mkdir(rpms, 0o755); err != nil {
		return err
	}
	db := filepath.Join(staging, "rpmdb")
	if strings.HasPrefix(key, "https://") {
		keyPath := filepath.Join(staging, "signing-key.gpg")
		if err := download(ctx, key, keyPath, 1024*1024); err != nil {
			return err
		}
		key = keyPath
	}
	if err := command(ctx, stderr, "rpmkeys", "--dbpath", db, "--import", key); err != nil {
		return err
	}
	for i, name := range names {
		selector := name + "-" + set.KernelRelease
		rows, err := queryRPMs(ctx, append(query, "repoquery", "--arch="+*architecture, "--queryformat=%{name}\t%{full_nevra}\t%{location}\t%{repoid}\n", selector), stderr)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf("kernel capture requires one matching %s RPM", selector)
		}
		fields := strings.Split(rows[0], "\t")
		if len(fields) != 4 || fields[0] != name || fields[1] != name+"-0:"+set.KernelRelease {
			return fmt.Errorf("kernel package %s does not match the selected build", name)
		}
		path := filepath.Join(rpms, selector+".rpm")
		if err := download(ctx, fields[2], path, 512*1024*1024); err != nil {
			return err
		}
		if err := command(ctx, stderr, "rpmkeys", "--define", "_pkgverify_level signature", "--dbpath", db, "--checksig", path); err != nil {
			return err
		}
		actual, err := exec.CommandContext(ctx, "rpm", "-qp", "--queryformat", "%{NAME}-%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH}", path).Output()
		if err != nil || string(actual) != fields[1] {
			return fmt.Errorf("downloaded RPM identity does not match %s", fields[1])
		}
		role := "runtime"
		if i == len(names)-1 {
			role = "build"
		}
		blob, err := payloadbundle.DescribeFile(path, role, kernelrelease.RPMMediaType, filepath.Base(path))
		if err != nil {
			return err
		}
		origin := fields[2]
		if *flavour == "standard" {
			// Record the canonical repository origin, independent of the mirror
			// selected by this discovery run. Byte verification uses RPM digests.
			switch fields[3] {
			case "fedora":
				origin = "https://dl.fedoraproject.org/pub/fedora/linux/releases/" + *fedora + "/Everything/" + *architecture + "/os/Packages/k/" + filepath.Base(path)
			case "updates":
				origin = "https://dl.fedoraproject.org/pub/fedora/linux/updates/" + *fedora + "/Everything/" + *architecture + "/Packages/k/" + filepath.Base(path)
			default:
				return fmt.Errorf("unexpected standard kernel repository %q", fields[3])
			}
		}
		set.Packages = append(set.Packages, kernelrelease.Package{Name: name, NEVRA: fields[1], SourceURL: origin, Descriptor: blob.Descriptor})
	}
	lock, err := kernelrelease.Export(ctx, set, rpms, filepath.Join(*output, "layout"), *repository)
	if err != nil {
		return err
	}
	if err := kernelrelease.Write(filepath.Join(*output, "lock.json"), lock); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(lock)
}

func queryRPMs(ctx context.Context, args []string, stderr io.Writer) ([]string, error) {
	cmd := exec.CommandContext(ctx, "dnf5", args...)
	cmd.Stderr = stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("query kernel RPMs: %w", err)
	}
	rows := strings.TrimSpace(string(data))
	if rows == "" {
		return nil, nil
	}
	return strings.Split(rows, "\n"), nil
}

func command(ctx context.Context, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func download(ctx context.Context, url, destination string, limit int64) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("kernel capture requires HTTPS origins")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 10 * time.Minute}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, response.Status)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(file, io.LimitReader(response.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if n > limit {
		return fmt.Errorf("kernel input exceeds download bound")
	}
	return closeErr
}
