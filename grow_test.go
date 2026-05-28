package partitionresizer

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/squashfs"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestCopyFilesystemsSquashfs builds a real squashfs in a source
// partition (using go-diskfs's squashfs writer), then runs
// copyFilesystems with a target partition that is three times the
// size of the source — the natural shape of a grow operation in a
// migration that's making room for a larger replacement rootfs.
// copyFilesystems takes the squashfs branch and calls
// sync.CopyPartitionRaw, which in turn calls sync.verifyBlockCopy.
//
// This test exercises:
//   - squashfs.Create / Finalize landing the filesystem at the source
//     partition's byte offset (not at byte 0 of the disk),
//   - copyFilesystems routing TypeSquashfs to the raw-copy branch,
//   - sync.verifyBlockCopy allowing target larger than source,
//   - squashfs.Read finding the copy at the target partition's offset
//     and walking it for the marker file.
func TestCopyFilesystemsSquashfs(t *testing.T) {
	workDir := t.TempDir()
	diskPath := filepath.Join(workDir, "disk.img")

	const (
		diskSize    int64 = 64 * MB
		sectorSize        = 4096 // squashfs requires blocksize >= 4096
		sourceStart       = 256  // sectors; 1 MiB into disk
		sourceSize        = 8 * MB
		// Target is 3x larger than source — the grow case.
		targetStart = sourceStart + (16 * MB / sectorSize)
		targetSize  = 24 * MB
		fileContent = "squashfs grow round-trip test\n"
		filename    = "marker.txt"
	)
	if err := os.WriteFile(diskPath, nil, 0o644); err != nil {
		t.Fatalf("create disk file: %v", err)
	}
	if err := os.Truncate(diskPath, diskSize); err != nil {
		t.Fatalf("size disk file: %v", err)
	}

	// First pass: write the partition table.
	bk, err := file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	d, err := diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		_ = bk.Close()
		t.Fatalf("open disk: %v", err)
	}
	table := &gpt.Table{
		LogicalSectorSize:  sectorSize,
		PhysicalSectorSize: sectorSize,
		Partitions: []*gpt.Partition{
			{Index: 1, Start: sourceStart, Size: sourceSize, Type: gpt.LinuxFilesystem, Name: "source"},
			{Index: 2, Start: targetStart, Size: targetSize, Type: gpt.LinuxFilesystem, Name: "target"},
		},
	}
	if err := d.Partition(table); err != nil {
		_ = bk.Close()
		t.Fatalf("write partition table: %v", err)
	}
	_ = bk.Close()

	// Reopen so partitions read from disk carry sector sizes from
	// disk — works around go-diskfs upstream gpt.Partition
	// sector-size propagation behavior.
	bk, err = file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("reopen backend: %v", err)
	}
	d, err = diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		_ = bk.Close()
		t.Fatalf("reopen disk: %v", err)
	}
	if _, err := d.GetPartitionTable(); err != nil {
		_ = bk.Close()
		t.Fatalf("re-read partition table: %v", err)
	}

	// Build a squashfs in the source partition with a known marker.
	srcFS, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 1, FSType: filesystem.TypeSquashfs})
	if err != nil {
		_ = bk.Close()
		t.Fatalf("CreateFilesystem(squashfs): %v", err)
	}
	rw, err := srcFS.OpenFile(filename, os.O_CREATE|os.O_RDWR)
	if err != nil {
		_ = bk.Close()
		t.Fatalf("OpenFile in source squashfs: %v", err)
	}
	if _, err := rw.Write([]byte(fileContent)); err != nil {
		_ = bk.Close()
		t.Fatalf("Write in source squashfs: %v", err)
	}
	sqs, ok := srcFS.(*squashfs.FileSystem)
	if !ok {
		_ = bk.Close()
		t.Fatalf("source not *squashfs.FileSystem")
	}
	if err := sqs.Finalize(squashfs.FinalizeOptions{
		NoCompressInodes:    true,
		NoCompressData:      true,
		NoCompressFragments: true,
	}); err != nil {
		_ = bk.Close()
		t.Fatalf("squashfs Finalize: %v", err)
	}
	_ = bk.Close()

	// Sanity: the GPT survived and the squashfs landed at the source
	// partition's byte offset.
	data, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read disk for sanity check: %v", err)
	}
	if !bytes.HasPrefix(data[sectorSize:sectorSize+8], []byte("EFI PART")) {
		t.Fatal("GPT primary header was corrupted by squashfs Finalize")
	}
	if !bytes.HasPrefix(data[sourceStart*sectorSize:sourceStart*sectorSize+4], []byte("hsqs")) {
		t.Fatalf("squashfs did not land at source partition offset %d", sourceStart*sectorSize)
	}

	// Reopen for the grow copy.
	bk, err = file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("reopen for copy: %v", err)
	}
	defer func() { _ = bk.Close() }()
	d, err = diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		t.Fatalf("open disk for copy: %v", err)
	}
	if _, err := d.GetPartitionTable(); err != nil {
		t.Fatalf("re-read table for copy: %v", err)
	}

	// Confirm copyFilesystems will take the squashfs branch.
	fsCheck, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem(source) before copy: %v", err)
	}
	if fsCheck.Type() != filesystem.TypeSquashfs {
		t.Fatalf("source filesystem is %v, expected squashfs", fsCheck.Type())
	}

	resizes := []partitionResizeTarget{
		{
			original: partitionData{
				number: 1,
				start:  sourceStart * sectorSize,
				size:   sourceSize,
				label:  "source",
			},
			target: partitionData{
				number: 2,
				start:  targetStart * sectorSize,
				size:   targetSize,
				label:  "target",
			},
		},
	}
	if err := copyFilesystems(d, resizes); err != nil {
		t.Fatalf("copyFilesystems (squashfs grow): %v", err)
	}

	// Read the new target back as squashfs and verify the marker file.
	dstFS, err := d.GetFilesystem(2)
	if err != nil {
		t.Fatalf("GetFilesystem(target) after copy: %v", err)
	}
	if dstFS.Type() != filesystem.TypeSquashfs {
		t.Fatalf("target filesystem is %v, expected squashfs", dstFS.Type())
	}
	mf, err := dstFS.OpenFile(filename, os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile in target squashfs: %v", err)
	}
	got, err := io.ReadAll(mf)
	if err != nil {
		t.Fatalf("read target marker: %v", err)
	}
	if string(got) != fileContent {
		t.Errorf("target marker mismatch: got %q, want %q", string(got), fileContent)
	}
}

// TestCopyFilesystemsFat32Grow exercises copyFilesystems' FAT32 path
// when growing into a larger target partition. The existing
// TestCopyFilesystems in resize_test.go uses an equal-sized target,
// which never tests the grow case.
//
// The FAT32 branch in copyFilesystems uses CreateFilesystem +
// CopyFileSystem + CompareFS rather than CopyPartitionRaw, so the
// EVE-style ESP grow workflow has to go through this code path. The
// test creates a small FAT32 source partition with known files, then
// grows it 4x and verifies the file content round-trips.
func TestCopyFilesystemsFat32Grow(t *testing.T) {
	workDir := t.TempDir()
	diskPath := filepath.Join(workDir, "disk.img")

	const (
		diskSize    int64 = 256 * MB
		sectorSize        = 512
		sourceStart       = 2048
		sourceSize        = 16 * MB
		targetStart       = sourceStart + (24 * MB / sectorSize)
		targetSize        = 64 * MB
		filename          = "MARKER.TXT"
		fileContent       = "FAT32 grow round-trip\n"
	)
	if err := os.WriteFile(diskPath, nil, 0o644); err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := os.Truncate(diskPath, diskSize); err != nil {
		t.Fatalf("size disk: %v", err)
	}

	bk, err := file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	d, err := diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		_ = bk.Close()
		t.Fatalf("open disk: %v", err)
	}
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{Index: 1, Start: sourceStart, Size: sourceSize, Type: gpt.EFISystemPartition, Name: "source"},
			{Index: 2, Start: targetStart, Size: targetSize, Type: gpt.EFISystemPartition, Name: "target"},
		},
	}
	if err := d.Partition(table); err != nil {
		_ = bk.Close()
		t.Fatalf("write partition table: %v", err)
	}
	_ = bk.Close()

	bk, err = file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("reopen backend: %v", err)
	}
	defer func() { _ = bk.Close() }()
	d, err = diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("reopen disk: %v", err)
	}
	if _, err := d.GetPartitionTable(); err != nil {
		t.Fatalf("re-read partition table: %v", err)
	}

	srcFS, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   1,
		FSType:      filesystem.TypeFat32,
		VolumeLabel: "source",
	})
	if err != nil {
		t.Fatalf("CreateFilesystem(fat32 source): %v", err)
	}
	rw, err := srcFS.OpenFile(filename, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile in source FAT32: %v", err)
	}
	if _, err := rw.Write([]byte(fileContent)); err != nil {
		t.Fatalf("Write to source: %v", err)
	}

	// Confirm copyFilesystems will take the FAT32 branch.
	srcCheck, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem(source) pre-copy: %v", err)
	}
	if srcCheck.Type() != filesystem.TypeFat32 {
		t.Fatalf("source filesystem is %v, expected FAT32", srcCheck.Type())
	}

	resizes := []partitionResizeTarget{
		{
			original: partitionData{
				number: 1,
				start:  sourceStart * sectorSize,
				size:   sourceSize,
				label:  "source",
			},
			target: partitionData{
				number: 2,
				start:  targetStart * sectorSize,
				size:   targetSize,
				label:  "target",
			},
		},
	}
	if err := copyFilesystems(d, resizes); err != nil {
		t.Fatalf("copyFilesystems (fat32 grow): %v", err)
	}

	dstFS, err := d.GetFilesystem(2)
	if err != nil {
		t.Fatalf("GetFilesystem(target) post-copy: %v", err)
	}
	if dstFS.Type() != filesystem.TypeFat32 {
		t.Fatalf("target filesystem is %v, expected FAT32", dstFS.Type())
	}
	mf, err := dstFS.OpenFile(filename, os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile in target FAT32: %v", err)
	}
	got, err := io.ReadAll(mf)
	if err != nil {
		t.Fatalf("read target marker: %v", err)
	}
	if string(got) != fileContent {
		t.Errorf("target marker mismatch: got %q, want %q", string(got), fileContent)
	}
}
