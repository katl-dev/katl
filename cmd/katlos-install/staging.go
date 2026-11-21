package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

const (
	// The installer has no swap, so a downloaded image occupies RAM until
	// the installer exits. /tmp is systemd's tmpfs sized at half of RAM;
	// /run is capped at a fifth, which is smaller than a KatlOS image on
	// small machines.
	imageStagingDir = "/tmp/katlos-image"
	// Headroom kept free after the download for the rest of the install.
	imageStagingReserve = 256 << 20
	// Keep in step with the installer memory minimum in docs/support.md.
	minimumInstallerRAMMiB = 2048
)

// memoryStagedResolver explains a staging refusal in terms of the
// machine's RAM, which is the only thing the operator can change.
type memoryStagedResolver struct {
	katlosimage.Resolver
	memInfo string
}

func newMemoryStagedResolver(resolver katlosimage.Resolver) memoryStagedResolver {
	resolver.WorkDir = imageStagingDir
	resolver.RequiredFreeBytes = imageStagingReserve
	staged := memoryStagedResolver{Resolver: resolver, memInfo: "/proc/meminfo"}
	staged.AvailableBytes = staged.availableBytes
	return staged
}

func (r memoryStagedResolver) ResolveKatlosImage(ctx context.Context, expected manifest.KatlosImage) (katlosimage.Payload, error) {
	payload, err := r.Resolver.ResolveKatlosImage(ctx, expected)
	var short *katlosimage.InsufficientStorageError
	if !errors.As(err, &short) {
		return payload, err
	}
	required := mebibytesCeil(short.RequiredBytes)
	available := short.AvailableBytes >> 20
	message := fmt.Sprintf("not enough memory to stage the KatlOS image: the installer keeps the download in RAM and needs %d MiB free, but %d MiB is available", required, available)
	if total, readErr := readMemInfo(r.memInfo, "MemTotal"); readErr == nil {
		message += fmt.Sprintf("; boot the installer on a machine with at least %d MiB of RAM", max(minimumInstallerRAMMiB, mebibytesCeil(total+short.RequiredBytes-short.AvailableBytes)))
	}
	return payload, memoryShortfallError{message: message + "; no disk changes were made", err: err}
}

// memoryShortfallError replaces the byte-level storage refusal in the
// operator's view while keeping it available to errors.As.
type memoryShortfallError struct {
	message string
	err     error
}

func (e memoryShortfallError) Error() string { return e.message }

func (e memoryShortfallError) Unwrap() error { return e.err }

// availableBytes is the smaller of the staging filesystem's free space and
// the memory that could back it.
func (r memoryStagedResolver) availableBytes(dir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("inspect %s: %w", dir, err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	memory, err := readMemInfo(r.memInfo, "MemAvailable")
	if err != nil {
		return 0, err
	}
	return min(free, memory), nil
}

func readMemInfo(path, field string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("read memory information: %w", err)
	}
	defer file.Close()
	value, err := parseMemInfo(file, field)
	if err != nil {
		return 0, fmt.Errorf("read memory information %s: %w", path, err)
	}
	return value, nil
}

func parseMemInfo(reader io.Reader, field string) (uint64, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok || name != field {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("%s has unexpected format %q", field, rest)
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", field, err)
		}
		return kib << 10, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s is missing", field)
}

func mebibytesCeil(bytes uint64) uint64 {
	return (bytes + 1<<20 - 1) >> 20
}
