package sysextimage

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckExtractor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		help    string
		wantErr string
	}{
		{name: "supports extract", help: " --extract[=X]          check if all files are well encoded, optionally extract to X"},
		{name: "predates extract", help: " -d#                    set output message level to # (maximum 9)", wantErr: "supports --extract"},
		{name: "absent", wantErr: "install erofs-utils"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.help != "" {
				// PATH holds only this script, so it uses shell builtins.
				script := "#!/bin/sh\nprintf '%s\\n' 'usage: [options] IMAGE' '" + tc.help + "'\n"
				if err := os.WriteFile(filepath.Join(bin, "fsck.erofs"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)

			err := CheckExtractor(t.Context())

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckExtractor() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckExtractor() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRootPartitionWithoutGPT(t *testing.T) {
	const size = 16 << 10
	image := strings.NewReader(strings.Repeat("\x00", size))

	if _, _, err := rootPartition(image, size); err == nil || !strings.Contains(err.Error(), "no GPT partition table") {
		t.Fatalf("rootPartition() error = %v, want no GPT partition table", err)
	}
}

const (
	rootType  = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
	linuxType = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
)

type testPartition struct {
	typ         string
	first, last uint64
}

func TestRootPartition(t *testing.T) {
	le := binary.LittleEndian
	setArray := func(count, entrySize uint32) func([]byte) {
		return func(header []byte) {
			le.PutUint32(header[80:84], count)
			le.PutUint32(header[84:88], entrySize)
		}
	}
	for _, tc := range []struct {
		name    string
		sector  int64
		sectors int64
		parts   []testPartition
		// editHeader changes header fields before the header checksum is
		// computed; corrupt changes the finished image.
		editHeader func(header []byte)
		corrupt    func(image []byte)
		wantOffset int64
		wantSize   int64
		wantErr    string
	}{
		{
			name: "512-byte sectors", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			wantOffset: 2 << 20, wantSize: 2 << 20,
		},
		{
			name: "4096-byte sectors", sector: 4096, sectors: 2048,
			parts:      []testPartition{{rootType, 512, 1023}},
			wantOffset: 2 << 20, wantSize: 2 << 20,
		},
		{
			name: "unrelated partition first", sector: 512, sectors: 16384,
			parts:      []testPartition{{linuxType, 2048, 6143}, {rootType, 6144, 8191}},
			wantOffset: 3 << 20, wantSize: 1 << 20,
		},
		{
			name: "root ending at the last sector", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 8192, 16383}},
			wantOffset: 4 << 20, wantSize: 4 << 20,
		},
		{
			name: "two roots", sector: 512, sectors: 16384,
			parts:   []testPartition{{rootType, 2048, 4095}, {rootType, 4096, 8191}},
			wantErr: "expected one x86-64 root partition, found 2",
		},
		{
			name: "no root", sector: 512, sectors: 16384,
			parts:   []testPartition{{linuxType, 2048, 4095}},
			wantErr: "expected one x86-64 root partition, found 0",
		},
		{
			name: "corrupt header checksum", sector: 512, sectors: 16384,
			parts:   []testPartition{{rootType, 4096, 8191}},
			corrupt: func(image []byte) { image[512+16] ^= 1 },
			wantErr: "GPT header checksum mismatch",
		},
		{
			name: "corrupt partition array", sector: 512, sectors: 16384,
			parts: []testPartition{{rootType, 4096, 8191}},
			// The name of the first entry, which the array checksum covers.
			corrupt: func(image []byte) { image[2*512+56] ^= 1 },
			wantErr: "GPT partition array checksum mismatch",
		},
		{
			name: "first LBA zero", sector: 512, sectors: 16384,
			parts:   []testPartition{{rootType, 0, 8191}},
			wantErr: "invalid root partition extent 0-8191",
		},
		{
			name: "last LBA before first", sector: 512, sectors: 16384,
			parts:   []testPartition{{rootType, 4096, 4095}},
			wantErr: "invalid root partition extent 4096-4095",
		},
		{
			name: "root past the image end", sector: 512, sectors: 16384,
			parts:   []testPartition{{rootType, 8192, 16384}},
			wantErr: "extends past the end",
		},
		{
			name: "root LBA overflowing an offset", sector: 512, sectors: 16384,
			parts:   []testPartition{{rootType, 1 << 62, 1<<62 + 1}},
			wantErr: "extends past the end",
		},
		{
			name: "array size overflowing", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: setArray(0xffffffff, 0xfffffff8),
			wantErr:    "invalid GPT partition array of",
		},
		{
			name: "array over 1 MiB", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: setArray(8193, 128),
			wantErr:    "invalid GPT partition array of",
		},
		{
			name: "entry over 4096 bytes", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: setArray(1, 4104),
			wantErr:    "invalid GPT partition array of",
		},
		{
			name: "entry size not a multiple of 8", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: setArray(128, 132),
			wantErr:    "invalid GPT partition array of",
		},
		{
			name: "array overlapping the header", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: func(header []byte) { le.PutUint64(header[72:80], 1) },
			wantErr:    "invalid GPT partition array LBA 1",
		},
		{
			name: "array past the image end", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: func(header []byte) { le.PutUint64(header[72:80], 1<<62) },
			wantErr:    "invalid GPT partition array LBA",
		},
		{
			name: "header not at LBA 1", sector: 512, sectors: 16384,
			parts:      []testPartition{{rootType, 4096, 8191}},
			editHeader: func(header []byte) { le.PutUint64(header[24:32], 2) },
			wantErr:    "own LBA as 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := gptImage(t, tc.sector, tc.sectors, tc.parts, tc.editHeader)
			if tc.corrupt != nil {
				tc.corrupt(image)
			}

			offset, size, err := rootPartition(bytes.NewReader(image), int64(len(image)))

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("rootPartition() = %d, %d, %v, want an error containing %q", offset, size, err, tc.wantErr)
				}
				return
			}
			if err != nil || offset != tc.wantOffset || size != tc.wantSize {
				t.Fatalf("rootPartition() = %d, %d, %v, want %d, %d", offset, size, err, tc.wantOffset, tc.wantSize)
			}
		})
	}
}

// gptImage returns an image of sectors sectors with a protective MBR and a
// primary GPT at LBA 1 whose 128-entry array at LBA 2 holds parts.
func gptImage(t *testing.T, sector, sectors int64, parts []testPartition, editHeader func([]byte)) []byte {
	t.Helper()
	le := binary.LittleEndian
	const count, entrySize = 128, 128
	image := make([]byte, sector*sectors)

	mbr := image[:512]
	mbr[446+4] = 0xee // A partition of type EFI GPT protective.
	le.PutUint32(mbr[446+8:], 1)
	le.PutUint32(mbr[446+12:], uint32(min(sectors-1, 0xffffffff)))
	mbr[510], mbr[511] = 0x55, 0xaa

	entries := image[2*sector : 2*sector+count*entrySize]
	for i, part := range parts {
		entry := entries[i*entrySize : (i+1)*entrySize]
		copy(entry[0:16], guidBytes(t, part.typ))
		entry[16] = byte(i + 1) // A distinct unique partition GUID.
		le.PutUint64(entry[32:40], part.first)
		le.PutUint64(entry[40:48], part.last)
	}

	header := image[sector : sector+92]
	copy(header[0:8], "EFI PART")
	le.PutUint32(header[8:12], 0x00010000)
	le.PutUint32(header[12:16], 92)
	le.PutUint64(header[24:32], 1)
	le.PutUint64(header[32:40], uint64(sectors-1))
	le.PutUint64(header[40:48], uint64(2+count*entrySize/sector))
	le.PutUint64(header[48:56], uint64(sectors-2-count*entrySize/sector))
	header[56] = 1 // A non-zero disk GUID.
	le.PutUint64(header[72:80], 2)
	le.PutUint32(header[80:84], count)
	le.PutUint32(header[84:88], entrySize)
	le.PutUint32(header[88:92], crc32.ChecksumIEEE(entries))
	if editHeader != nil {
		editHeader(header)
	}
	le.PutUint32(header[16:20], crc32.ChecksumIEEE(header))
	return image
}

// guidBytes returns the on-disk form of a GUID, whose first three fields are
// little-endian.
func guidBytes(t *testing.T, guid string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(guid, "-", ""))
	if err != nil || len(b) != 16 {
		t.Fatalf("invalid GUID %q: %v", guid, err)
	}
	slices.Reverse(b[0:4])
	slices.Reverse(b[4:6])
	slices.Reverse(b[6:8])
	return b
}
