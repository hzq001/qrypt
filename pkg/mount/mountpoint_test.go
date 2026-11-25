package mount

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	testclock "github.com/yinzhenyu/qrypt/pkg/clock"
)

func TestEnsureMountPointCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "Qrypt")

	if err := ensureMountPoint(path); err != nil {
		t.Fatalf("ensureMountPoint: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat mount point: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("mount point is not a directory: %v", info.Mode())
	}
}

func TestEnsureMountPointAcceptsExistingDirectory(t *testing.T) {
	// A clean unmount leaves the directory behind, and FUSE mounts over a
	// non-empty directory are rejected by the kernel, not by this function.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("seed directory: %v", err)
	}

	if err := ensureMountPoint(dir); err != nil {
		t.Fatalf("ensureMountPoint on existing directory: %v", err)
	}
}

func TestEnsureMountPointRejectsNonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Qrypt")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	err := ensureMountPoint(path)
	if err == nil {
		t.Fatal("ensureMountPoint accepted a regular file as a mount point")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
}

func TestEnsureMountPointAcceptsPlainDirectory(t *testing.T) {
	// The macOS dead-mount detection runs on every successfully created mount
	// point; an ordinary directory must not be mistaken for one.
	dir := t.TempDir()

	if root, dead := deadFuseMountRoot(dir); dead {
		t.Fatalf("deadFuseMountRoot(%s) reported the mount root %q as dead", dir, root)
	}
	if err := ensureMountPoint(dir); err != nil {
		t.Fatalf("ensureMountPoint on a plain directory: %v", err)
	}
}

func TestIsFuseFilesystem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		fsType string
		source string
		want   bool
	}{
		{name: "macfuse", fsType: "macfuse", want: true},
		{name: "osxfuse", fsType: "osxfuse", want: true},
		{name: "linux subtype", fsType: "fuse.qrypt", want: true},
		{name: "fuseblk", fsType: "fuseblk", want: true},
		{name: "case insensitive", fsType: "macFUSE", want: true},
		// FUSE-T exports a localhost NFS mount: the type is plain "nfs" and
		// only the source names FUSE-T. Rejecting this case is what left a
		// killed FUSE-T mount unrecognised, so the next mount failed on it.
		{name: "fuse-t over nfs", fsType: "nfs", source: "fuse-t:/Qrypt", want: true},
		{name: "fuse-t source case", fsType: "nfs", source: "FUSE-T:/Qrypt", want: true},
		{name: "real nfs share", fsType: "nfs", source: "server:/export/share", want: false},
		{name: "apfs", fsType: "apfs", source: "/dev/disk3s5", want: false},
		{name: "nfs without source", fsType: "nfs", want: false},
		{name: "empty", fsType: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFuseFilesystem(tc.fsType, tc.source); got != tc.want {
				t.Fatalf("isFuseFilesystem(%q, %q) = %v, want %v", tc.fsType, tc.source, got, tc.want)
			}
		})
	}
}

func TestDetachRootForPlainDirectoryIsThePathItself(t *testing.T) {
	t.Parallel()
	// Nothing is mounted on an ordinary directory, so there is no mount root to
	// prefer over the path the user asked for.
	dir := t.TempDir()

	if got := detachRootFor(dir); got != dir {
		t.Fatalf("detachRootFor(%s) = %q, want the path itself", dir, got)
	}
}

func TestBoundedProbeReturnsTheProbeAnswer(t *testing.T) {
	t.Parallel()
	want := errors.New("device not configured")

	finished, err := boundedProbe(testclock.Real, time.Second, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("boundedProbe = %v, want %v", err, want)
	}
	if !finished {
		t.Fatal("boundedProbe reported a timeout for a probe that answered")
	}
}

func TestBoundedProbeGivesUpOnASilentProbe(t *testing.T) {
	t.Parallel()
	// A hung FUSE daemon must not look dead: force-detaching a working mount
	// would be worse than the failure the user already has. The probe timeout
	// runs on the fake clock, so the test proves the deadline fired without
	// waiting the 50ms out.
	clk := testclock.NewFake(time.Now())
	blocked := make(chan struct{})
	defer close(blocked)

	type probeOutcome struct {
		finished bool
		err      error
	}
	result := make(chan probeOutcome, 1)
	go func() {
		finished, err := boundedProbe(clk, 50*time.Millisecond, func() error { <-blocked; return errors.New("never") })
		result <- probeOutcome{finished: finished, err: err}
	}()
	// Wait for boundedProbe to arm its timeout (pending fake timers are only
	// observable through the clock) without sleeping.
	deadline := time.Now().Add(5 * time.Second)
	for clk.Pending() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if clk.Pending() == 0 {
		t.Fatal("boundedProbe never armed its timeout")
	}
	clk.Advance(50 * time.Millisecond)
	select {
	case out := <-result:
		if out.err != nil {
			t.Fatalf("boundedProbe on a silent probe = %v, want nil (inconclusive)", out.err)
		}
		if out.finished {
			t.Fatal("boundedProbe reported completion for a probe it gave up on")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("boundedProbe did not give up after the timeout elapsed")
	}
}
