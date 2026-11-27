// Package sysextimage reads the file system of a system extension image on
// a build or test host, without mounting it or elevating privileges.
//
// It owns only unprivileged extraction of a sysext's root partition;
// internal/kernelmodule mounts kernel-extension sysexts with systemd-dissect
// for depmod, which is a different contract.
package sysextimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"slices"
)

// rootX86_64 is the discoverable-partitions type GUID
// 4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709 in its on-disk mixed-endian form.
var rootX86_64 = [16]byte{0xe3, 0xbc, 0x68, 0x4f, 0xcd, 0xe8, 0xb1, 0x4d, 0x96, 0xe7, 0xfb, 0xca, 0xf9, 0x84, 0xb7, 0x09}

// An image file carries no logical sector size, so the GPT header is looked
// for at LBA 1 for each size images are written with.
var sectorSizes = []int64{512, 4096}

// Extract writes the files of image's one x86-64 root partition into dir,
// creating dir and replacing files already there.
func Extract(ctx context.Context, image, dir string) error {
	fsck, err := extractor(ctx)
	if err != nil {
		return err
	}

	file, err := os.Open(image)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset, size, err := rootPartition(file, info.Size())
	if err != nil {
		return fmt.Errorf("%s: %w", image, err)
	}

	// The partition is copied out because older fsck.erofs, such as Ubuntu
	// 24.04's 1.7, cannot read a file system at an offset.
	part, err := os.CreateTemp("", "katl-sysext-root-*.erofs")
	if err != nil {
		return err
	}
	defer os.Remove(part.Name())
	copied, err := io.Copy(part, io.NewSectionReader(file, offset, size))
	closeErr := part.Close()
	if err != nil {
		return fmt.Errorf("copy root partition of %s: %w", image, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if copied != size {
		return fmt.Errorf("%s: root partition extends past the end of the image", image)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, fsck, "--extract="+dir, "--overwrite", "--no-preserve", part.Name())
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("extract root partition of %s: %w: %s", image, err, output)
	}
	return nil
}

// CheckExtractor reports why this host cannot run Extract, or nil if it can.
func CheckExtractor(ctx context.Context) error {
	_, err := extractor(ctx)
	return err
}

func extractor(ctx context.Context) (string, error) {
	path, err := exec.LookPath("fsck.erofs")
	if err != nil {
		return "", fmt.Errorf("fsck.erofs is required to read system extension images; install erofs-utils: %w", err)
	}
	// fsck.erofs predates --extract, and a version without it would only
	// reject the option once the image had been copied.
	output, _ := exec.CommandContext(ctx, path, "--help").CombinedOutput()
	if !bytes.Contains(output, []byte("--extract")) {
		return "", fmt.Errorf("%s cannot extract images; install an erofs-utils whose fsck.erofs supports --extract", path)
	}
	return path, nil
}

// rootPartition returns the byte offset and size of the one x86-64 root
// partition in the GPT of r, an image of imageSize bytes. The partition lies
// within the image.
func rootPartition(r io.ReaderAt, imageSize int64) (offset, size int64, err error) {
	for _, sector := range sectorSizes {
		header := make([]byte, sector)
		if _, err := r.ReadAt(header, sector); err != nil && !errors.Is(err, io.EOF) {
			return 0, 0, err
		}
		if string(header[:8]) == "EFI PART" {
			return rootInTable(r, imageSize, sector, header)
		}
	}
	return 0, 0, errors.New("no GPT partition table")
}

func rootInTable(r io.ReaderAt, imageSize, sector int64, header []byte) (offset, size int64, err error) {
	le := binary.LittleEndian
	headerSize := int64(le.Uint32(header[12:16]))
	if headerSize < 92 || headerSize > sector {
		return 0, 0, fmt.Errorf("invalid GPT header size %d", headerSize)
	}
	checked := bytes.Clone(header[:headerSize])
	clear(checked[16:20])
	if crc32.ChecksumIEEE(checked) != le.Uint32(header[16:20]) {
		return 0, 0, errors.New("GPT header checksum mismatch")
	}

	// Each LBA and size is bounded by the image before it is scaled, so
	// crafted header fields cannot overflow an offset or an allocation.
	imageSectors := uint64(imageSize / sector)
	if myLBA := le.Uint64(header[24:32]); myLBA != 1 {
		return 0, 0, fmt.Errorf("GPT header records its own LBA as %d, want 1", myLBA)
	}
	entriesLBA := le.Uint64(header[72:80])
	if entriesLBA < 2 || entriesLBA >= imageSectors {
		return 0, 0, fmt.Errorf("invalid GPT partition array LBA %d", entriesLBA)
	}
	count := int64(le.Uint32(header[80:84]))
	entrySize := int64(le.Uint32(header[84:88]))
	if entrySize < 128 || entrySize > 4096 || entrySize%8 != 0 || count > (1<<20)/entrySize {
		return 0, 0, fmt.Errorf("invalid GPT partition array of %d entries of %d bytes", count, entrySize)
	}
	entries := make([]byte, count*entrySize)
	if _, err := r.ReadAt(entries, int64(entriesLBA)*sector); err != nil {
		return 0, 0, fmt.Errorf("read GPT partition array: %w", err)
	}
	if crc32.ChecksumIEEE(entries) != le.Uint32(header[88:92]) {
		return 0, 0, errors.New("GPT partition array checksum mismatch")
	}

	// Select by type so unrelated partitions and their order are immaterial.
	roots := 0
	for entry := range slices.Chunk(entries, int(entrySize)) {
		if [16]byte(entry[:16]) != rootX86_64 {
			continue
		}
		roots++
		first, last := le.Uint64(entry[32:40]), le.Uint64(entry[40:48])
		if first == 0 || last < first {
			return 0, 0, fmt.Errorf("invalid root partition extent %d-%d", first, last)
		}
		if last >= imageSectors {
			return 0, 0, fmt.Errorf("root partition extent %d-%d extends past the end of the %d-sector image", first, last, imageSectors)
		}
		offset, size = int64(first)*sector, int64(last-first+1)*sector
	}
	if roots != 1 {
		return 0, 0, fmt.Errorf("expected one x86-64 root partition, found %d", roots)
	}
	return offset, size, nil
}
