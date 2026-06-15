package partitionresizer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestShrinkPartitionsSparseIndex verifies shrinkPartitions on a non-contiguous
// GPT layout, where a partition's number is not its slice position + 1 (e.g.
// EVE's persist partition at index 9 with gaps below it). d.Table.Partitions is
// a compacted slice of only the active partitions, so the previous
// table.Partitions[number-1] indexing either touched the wrong entry or
// panicked with index-out-of-range. The fix looks the partition up by Index.
func TestShrinkPartitionsSparseIndex(t *testing.T) {
	const sector = 512
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "disk.img")
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("create disk image: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(256 * MB); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	d, err := diskfs.OpenBackend(file.New(f, false), diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}

	p1Start := uint64(2048)
	p3Start := p1Start + 36*MB/sector
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{Index: 1, Start: p1Start, Size: 36 * MB, Type: gpt.LinuxFilesystem, Name: "p1"},
			// index 9, slice position 1: the case that broke table.Partitions[8]
			{Index: 9, Start: p3Start, Size: 128 * MB, Type: gpt.LinuxFilesystem, Name: "P3"},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("write partition table: %v", err)
	}

	// shrink P3 (index 9) from 128 MB to 64 MB
	resizes := []partitionResizeTarget{{
		original: partitionData{number: 9, label: "P3", size: 128 * MB},
		target:   partitionData{number: 9, size: 64 * MB},
	}}
	if err := shrinkPartitions(d, resizes); err != nil {
		t.Fatalf("shrinkPartitions failed: %v", err)
	}

	tr, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get partition table: %v", err)
	}
	byIndex := make(map[int]*gpt.Partition)
	for _, p := range tr.(*gpt.Table).Partitions {
		if p.Type == gpt.Unused {
			continue
		}
		byIndex[p.Index] = p
	}
	if got := int64(byIndex[9].GetSize()); got != 64*MB {
		t.Errorf("P3 (index 9) size = %d, want %d", got, 64*MB)
	}
	// the partition at slice position 8 does not exist; the partition that the
	// old code would have touched (index 1) must be left alone
	if got := int64(byIndex[1].GetSize()); got != 36*MB {
		t.Errorf("p1 (index 1) size = %d, want %d (must be untouched)", got, 36*MB)
	}
}
