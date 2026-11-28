package generation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/systemdisk"
)

type bootabilityState uint8

const (
	bootIndeterminate bootabilityState = iota
	bootUnavailable
	bootable
)

type bootabilityLevel uint8

const (
	bootArtifacts bootabilityLevel = iota
	bootIdentity
	bootFull
)

type bootability struct {
	state    bootabilityState
	evidence string
	err      error
	digest   string
}

func (result bootability) problem() error {
	switch result.state {
	case bootIndeterminate:
		return fmt.Errorf("bootability indeterminate: %w", result.err)
	case bootUnavailable:
		return fmt.Errorf("cannot boot: %s", result.evidence)
	default:
		return nil
	}
}

// CheckBootability reports why a generation cannot be verified as bootable.
// It supports read-only guidance for a published generation.
func CheckBootability(root string, spec GenerationSpec, status GenerationStatus) error {
	check := bootabilityCheck{root: root}
	return check.check(spec, status, bootFull).problem()
}

type bootabilityCheck struct {
	root  string
	roots map[string]bootability
}

// check verifies the artifacts selected by an immutable generation spec.
// Callers apply their own health and transition requirements.
func (check *bootabilityCheck) check(spec GenerationSpec, status GenerationStatus, level bootabilityLevel) bootability {
	if status.UnavailableReason != "" {
		return bootability{state: bootUnavailable, evidence: status.UnavailableReason}
	}
	if files := checkBootFiles(check.root, spec); files.state != bootable {
		return files
	}
	if level == bootArtifacts || level == bootFull {
		if extensions := checkBootExtensions(check.root, spec); extensions.state != bootable || level == bootArtifacts {
			return extensions
		}
	}
	uuid := strings.ToLower(spec.Root.PartitionUUID)
	if level == bootIdentity {
		return checkRootArtifact(check.root, uuid, level)
	}

	// A cleanup pass can inspect several generations on the same slot.
	// Share the observed digest for that pass, under the boot-state lock.
	if check.roots == nil {
		check.roots = make(map[string]bootability)
	}
	observed, ok := check.roots[uuid]
	if !ok {
		observed = checkRootArtifact(check.root, uuid, level)
		check.roots[uuid] = observed
	}
	if observed.state != bootable {
		return observed
	}
	if !strings.EqualFold(spec.Root.RuntimeArtifactSHA256, observed.digest) {
		observed.state = bootUnavailable
		observed.evidence = fmt.Sprintf("root PARTUUID %s has runtime SHA-256 %s, expected %s", uuid, observed.digest, spec.Root.RuntimeArtifactSHA256)
	}
	return observed
}

func checkBootExtensions(root string, spec GenerationSpec) bootability {
	for _, extensions := range [][]ExtensionRef{spec.Sysexts, spec.BundledConfexts} {
		for _, ref := range extensions {
			if file := checkBootArtifact(root, ref.Path, false); file.state != bootable {
				return file
			}
		}
	}
	for _, ref := range spec.Confexts {
		if directory := checkBootArtifact(root, ref.Path, true); directory.state != bootable {
			return directory
		}
	}
	return bootability{state: bootable}
}

func checkBootFiles(root string, spec GenerationSpec) bootability {
	if spec.Boot.LoaderEntryPath == "" {
		return bootability{state: bootUnavailable, evidence: "loader entry is missing"}
	}
	if entry := checkBootArtifact(root, filepath.Join("/efi", spec.Boot.LoaderEntryPath), false); entry.state != bootable {
		return entry
	}
	return checkBootArtifact(root, filepath.Join("/efi", entryPath(spec.Boot.UKIPath)), false)
}

func checkBootArtifact(root, path string, directory bool) bootability {
	info, err := os.Stat(rootedPathUnchecked(root, path))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return bootability{state: bootUnavailable, evidence: "boot artifact unavailable: " + path}
	case err != nil:
		return bootability{state: bootIndeterminate, err: fmt.Errorf("check boot artifact %s: %w", path, err)}
	case (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()):
		return bootability{state: bootUnavailable, evidence: "boot artifact has unexpected type: " + path}
	default:
		return bootability{state: bootable, evidence: path}
	}
}

func checkRootArtifact(root, uuid string, level bootabilityLevel) bootability {
	layout, err := systemdisk.ResolveRunning(cleanRoot(root))
	if err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("locate root PARTUUID %s: %w", uuid, err)}
	}
	partition, found, err := layout.BootPartition(uuid)
	if err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("locate root PARTUUID %s: %w", uuid, err)}
	}
	if !found {
		return bootability{state: bootUnavailable, evidence: "root PARTUUID " + uuid + " is absent from the system disk"}
	}
	if level == bootIdentity {
		// Boot activation can inspect sysfs under PrivateDevices=yes.
		return bootability{state: bootable}
	}
	node, err := partition.Node()
	if err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("locate bound root PARTUUID %s: %w", uuid, err)}
	}
	file, err := os.Open(rootedPathUnchecked(root, node))
	if err != nil {
		// A bound partition can become inaccessible between lookup and open.
		return bootability{state: bootIndeterminate, err: fmt.Errorf("open root PARTUUID %s: %w", uuid, err)}
	}
	defer file.Close()
	capacity, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("read size of root PARTUUID %s: %w", uuid, err)}
	}
	return readRootArtifact(file, uuid, capacity)
}

func readRootArtifact(reader io.ReaderAt, uuid string, capacity int64) bootability {
	// Katl ships mksquashfs's default 4 KiB-padded image. The superblock
	// records its unpadded bytes_used at offset 40. The slot can be larger
	// than the image and still contain data from an earlier transfer.
	const superblockSize, padding = 96, 4096
	var header [superblockSize]byte
	if _, err := reader.ReadAt(header[:], 0); err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("read root PARTUUID %s superblock: %w", uuid, err)}
	}
	used := binary.LittleEndian.Uint64(header[40:48])
	if string(header[:4]) != "hsqs" {
		return bootability{state: bootUnavailable, evidence: "root PARTUUID " + uuid + " has no valid SquashFS superblock"}
	}
	if used < superblockSize || used > uint64(capacity&^(padding-1)) {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("root PARTUUID %s reports implausible SquashFS bytes_used %d for a %d-byte partition", uuid, used, capacity)}
	}
	size := int64((used + padding - 1) &^ (padding - 1))
	hash := sha256.New()
	n, err := io.Copy(hash, io.NewSectionReader(reader, 0, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return bootability{state: bootIndeterminate, err: fmt.Errorf("read runtime image on root PARTUUID %s: %w", uuid, err)}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	return bootability{state: bootable, evidence: fmt.Sprintf("root PARTUUID %s has runtime SHA-256 %s", uuid, digest), digest: digest}
}
