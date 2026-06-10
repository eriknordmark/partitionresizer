package partitionresizer

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
)

// wholeDiskSum streams a sha256 over the entire disk image.
func wholeDiskSum(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash disk: %v", err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// --- #3: untouched partitions stay byte-identical ----------------------------

// TestUntouchedPartitionsByteIntegrity verifies that the partitions a grow
// must not touch -- persist (P9, user data) and CONFIG -- are byte-for-byte
// unchanged across Case 2, and that P9's filesystem data survives.
func TestUntouchedPartitionsByteIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	if err := Run(fx.path, nil, shrinkP9, false, false, false); err != nil {
		t.Fatalf("Case 1 (shrink P9): %v", err)
	}
	p9Len := int((defaultP9MB - 600) * MB)
	cfgLen := int(configMB * MB)
	p9Before := partitionRegionSum(t, fx.path, fx.p9Start, p9Len)
	cfgBefore := partitionRegionSum(t, fx.path, fx.configStart, cfgLen)

	if err := Run(fx.path, nil, growImages, false, false, true); err != nil {
		t.Fatalf("Case 2 (grow): %v", err)
	}

	if partitionRegionSum(t, fx.path, fx.p9Start, p9Len) != p9Before {
		t.Error("P9 (persist) region changed during the grow")
	}
	if partitionRegionSum(t, fx.path, fx.configStart, cfgLen) != cfgBefore {
		t.Error("CONFIG region changed during the grow")
	}
	if got := readPartitionFile(t, fx.path, p9Index, "/p9-marker.txt"); got != "persist content" {
		t.Errorf("P9 data lost: marker = %q", got)
	}
}

// --- #4: a failure leaves the disk unmodified --------------------------------

// TestInsufficientSpaceIsAtomic verifies that a grow which cannot fit (no
// free space and no shrink partition) fails without modifying the disk, so a
// reboot inherits the original state, not a half-applied one.
func TestInsufficientSpaceIsAtomic(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	before := wholeDiskSum(t, fx.path)

	// full disk, grows that need ~496 MB, no shrink partition supplied
	err := Run(fx.path, nil, growImages, false, false, true)
	if err == nil {
		t.Fatal("expected an insufficient-space error, got nil")
	}
	if wholeDiskSum(t, fx.path) != before {
		t.Error("disk was modified despite the planning failure (not atomic)")
	}
	t.Logf("aborted cleanly: %v", err)
}

// --- #5: refuse to shrink below the filesystem's used size -------------------

// TestShrinkBelowUsedAborts verifies that asking to shrink P9 below its ext4
// used size makes e2fsck/resize2fs refuse, and the resize aborts with the
// partition and its data untouched.
func TestShrinkBelowUsedAborts(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	p9Before := partitionRegionSum(t, fx.path, fx.p9Start, int(defaultP9MB*MB))

	// 16 MB is well below P9's used size; resize2fs must refuse
	err := Run(fx.path, nil, []PartitionChange{NewPartitionChange(IdentifierByLabel, "P9", 16*MB)}, false, false, false)
	if err == nil {
		t.Fatal("expected resize2fs to refuse shrinking below used size, got nil")
	}
	after := gptByName(t, fx.path)
	if got := int64(after["P9"].GetSize()); got != defaultP9MB*MB {
		t.Errorf("P9 size changed to %d despite the failed shrink (want %d)", got, defaultP9MB*MB)
	}
	if partitionRegionSum(t, fx.path, fx.p9Start, int(defaultP9MB*MB)) != p9Before {
		t.Error("P9 region changed despite the failed shrink")
	}
	if got := readPartitionFile(t, fx.path, p9Index, "/p9-marker.txt"); got != "persist content" {
		t.Errorf("P9 data lost: marker = %q", got)
	}
	t.Logf("aborted cleanly: %v", err)
}

// --- #6: combined shrink + grow in a single Run ------------------------------

// TestCombinedShrinkGrow exercises the natural one-shot usage: a single
// Run that shrinks P9 to make room and grows ESP/IMGA/IMGB. go-diskfs rounds
// the shrink up to a whole GB, so this uses a larger P9 (1400 MB) than the
// default fixture.
func TestCombinedShrinkGrow(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	const p9MB = 1400
	const diskMB = p9MB + 252 // front partitions (~250 MB) + margin
	fx := buildSampleLayoutSized(t, diskMB, p9MB)

	shrink := NewPartitionIdentifier(IdentifierByLabel, "P9")
	if err := Run(fx.path, &shrink, growImages, false, false, true); err != nil {
		t.Fatalf("combined shrink+grow Run: %v", err)
	}

	after := gptByName(t, fx.path)
	// P9 shrunk by the GB-rounded total grow (496 MB -> 1 GB)
	if got := int64(after["P9"].GetSize()); got != (p9MB-1024)*MB {
		t.Errorf("P9 size = %d, want %d", got, (p9MB-1024)*MB)
	}
	if after["ESP"].Index != espIndex || after["IMGA"].Index != imgaIndex || after["IMGB"].Index != imgbIndex {
		t.Errorf("indices not preserved: ESP=%d IMGA=%d IMGB=%d", after["ESP"].Index, after["IMGA"].Index, after["IMGB"].Index)
	}
	if after["IMGA"].Start <= after["P9"].Start {
		t.Errorf("IMGA (%d) not relocated past P9 (%d)", after["IMGA"].Start, after["P9"].Start)
	}
	if int64(after["ESP"].GetSize()) != 96*MB || int64(after["IMGA"].GetSize()) != 200*MB || int64(after["IMGB"].GetSize()) != 200*MB {
		t.Errorf("grown sizes wrong: ESP=%d IMGA=%d IMGB=%d", after["ESP"].GetSize(), after["IMGA"].GetSize(), after["IMGB"].GetSize())
	}
	if got := partitionRegionSum(t, fx.path, after["IMGA"].Start, 1*MB); got != fx.imgaSum {
		t.Error("IMGA content not preserved")
	}
}

// --- #7: realistic P9 content survives the shrink ----------------------------

// TestShrinkPreservesP9Content fills P9 with many files (so resize2fs has
// real data to relocate during the shrink), shrinks it, then verifies every
// file is intact.
func TestShrinkPreservesP9Content(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	want := populateP9(t, fx.path, 30)

	// shrink P9 from 900 MB to 200 MB (above used; forces block relocation)
	if err := Run(fx.path, nil, []PartitionChange{NewPartitionChange(IdentifierByLabel, "P9", 200*MB)}, false, false, false); err != nil {
		t.Fatalf("shrink P9: %v", err)
	}
	after := gptByName(t, fx.path)
	if got := int64(after["P9"].GetSize()); got != 200*MB {
		t.Errorf("P9 size = %d, want %d", got, 200*MB)
	}
	for name, content := range want {
		if got := readPartitionFile(t, fx.path, p9Index, name); got != content {
			t.Errorf("P9 file %s content mismatch after shrink (len got=%d want=%d)", name, len(got), len(content))
		}
	}
}

// populateP9 writes n distinct files into the P9 filesystem and returns their
// expected contents.
func populateP9(t *testing.T, path string, n int) map[string]string {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, false), diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	fs, err := d.GetFilesystem(p9Index)
	if err != nil {
		t.Fatalf("get P9 filesystem: %v", err)
	}
	want := make(map[string]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/p9file-%02d.dat", i)
		// a few KB of deterministic, per-file content
		content := fmt.Sprintf("file %d:", i)
		for len(content) < 4096 {
			content += fmt.Sprintf("%d-%x;", i, len(content))
		}
		fh, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fh.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := fh.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
		want[name] = content
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return want
}

// --- #1 + #2: SIGKILL chaos --------------------------------------------------

// TestChaosKill runs the real resizer binary doing the sample-layout grow and SIGKILLs
// it at random moments, re-running until it completes. This exercises resume
// from arbitrary interruption points -- including partial GPT writes and
// half-copied partition content -- across repeated kills, and asserts that the
// final disk is correct and that persist/CONFIG are never corrupted.
//
// SIGKILL models a process crash (writes already issued survive); it is not a
// full power-loss model (no torn sectors / lost un-fsync'd writes).
func TestChaosKill(t *testing.T) {
	if testing.Short() {
		t.Skip("slow chaos test")
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not available")
	}
	// build the resizer binary
	bin := filepath.Join(t.TempDir(), "resizer")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/resizer").CombinedOutput(); err != nil {
		t.Fatalf("build resizer: %v\n%s", err, out)
	}

	// base disk: P9 pre-shrunk so the grow has free space to work in
	base := buildSampleLayout(t)
	if err := Run(base.path, nil, shrinkP9, false, false, false); err != nil {
		t.Fatalf("pre-shrink: %v", err)
	}
	p9Len := int((defaultP9MB - 600) * MB)
	cfgLen := int(configMB * MB)
	p9Sum := partitionRegionSum(t, base.path, base.p9Start, p9Len)
	cfgSum := partitionRegionSum(t, base.path, base.configStart, cfgLen)

	growArgs := []string{
		"--preserve-numbers",
		"--grow-partition", "label:ESP:96M",
		"--grow-partition", "label:IMGA:200M",
		"--grow-partition", "label:IMGB:200M",
	}
	rng := rand.New(rand.NewSource(1))
	const trials = 5
	for trial := 0; trial < trials; trial++ {
		disk := filepath.Join(t.TempDir(), "disk.img")
		if err := testCopyFile(base.path, disk); err != nil {
			t.Fatalf("copy base disk: %v", err)
		}
		kills := runGrowWithRandomKills(t, bin, append(growArgs, disk), rng)

		after := gptByName(t, disk)
		if after["ESP"].Index != espIndex || after["IMGA"].Index != imgaIndex || after["IMGB"].Index != imgbIndex {
			t.Errorf("trial %d: indices not preserved", trial)
		}
		if int64(after["IMGA"].GetSize()) != 200*MB || int64(after["IMGB"].GetSize()) != 200*MB || int64(after["ESP"].GetSize()) != 96*MB {
			t.Errorf("trial %d: grown sizes wrong", trial)
		}
		if partitionRegionSum(t, disk, after["IMGA"].Start, 1*MB) != base.imgaSum ||
			partitionRegionSum(t, disk, after["IMGB"].Start, 1*MB) != base.imgbSum {
			t.Errorf("trial %d: IMG content not preserved", trial)
		}
		if partitionRegionSum(t, disk, base.p9Start, p9Len) != p9Sum {
			t.Errorf("trial %d: P9 (persist) corrupted by interrupted resize", trial)
		}
		if partitionRegionSum(t, disk, base.configStart, cfgLen) != cfgSum {
			t.Errorf("trial %d: CONFIG corrupted by interrupted resize", trial)
		}
		t.Logf("trial %d converged after %d kill(s)", trial, kills)
	}
}

// runGrowWithRandomKills spawns the resizer, SIGKILLs it after a random delay,
// and re-runs until it completes successfully; returns the number of kills. A
// non-zero exit that we did NOT cause is a real failure (resume must always be
// able to finish).
func runGrowWithRandomKills(t *testing.T, bin string, args []string, rng *rand.Rand) int {
	t.Helper()
	// SIGKILL a random number of times at varied points, then run once
	// un-killed; that final run must complete (the idempotency assertion).
	killsWanted := rng.Intn(4) + 1
	kills := 0
	for attempt := 0; attempt < 80; attempt++ {
		cmd := exec.Command(bin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start resizer: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		if kills >= killsWanted {
			// guaranteed completion run
			if werr := <-done; werr != nil {
				t.Fatalf("resume run failed after %d kill(s) (resume not safe): %v\nstderr:\n%s", kills, werr, stderr.String())
			}
			return kills
		}
		delay := time.Duration(rng.Intn(2500)+50) * time.Millisecond
		select {
		case werr := <-done:
			if werr == nil {
				return kills // finished before we killed it
			}
			t.Fatalf("resizer exited with error without being killed (resume not safe): %v\nstderr:\n%s", werr, stderr.String())
		case <-time.After(delay):
			_ = cmd.Process.Kill()
			<-done
			kills++
		}
	}
	t.Fatalf("resize did not converge within the attempt cap")
	return kills
}
