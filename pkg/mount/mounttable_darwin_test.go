//go:build darwin

package mount

import (
	"os"
	"path/filepath"
	"testing"
)

// withMountTable replaces the kernel mount table for one test. The seam exists
// so the session-end classification can be exercised without mounting anything.
func withMountTable(t *testing.T, mountPoints ...string) {
	t.Helper()
	previous := mountTableMountPoints
	mountTableMountPoints = func() []string { return mountPoints }
	t.Cleanup(func() { mountTableMountPoints = previous })
}

func TestMountedAtFollowsTheMountTable(t *testing.T) {
	// The distinction this test pins is the whole bug: when the mount point is
	// gone the session ended because something outside qrypt unmounted it, and
	// the CLI must not report that as a failure (a non-zero exit makes the
	// service manager remount the volume the user just ejected).
	dir := filepath.Join(t.TempDir(), "Qrypt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The mount table records resolved paths (/var is a symlink to
	// /private/var on macOS).
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	withMountTable(t, "/", resolved)
	if !mountedAt(dir) {
		t.Fatalf("mountedAt(%q) = false while the mount table lists it", dir)
	}

	// An external unmount removes the entry while the directory remains.
	withMountTable(t, "/")
	if mountedAt(dir) {
		t.Fatalf("mountedAt(%q) = true after the mount table dropped it", dir)
	}
}

func TestMountedAtIgnoresPrefixAndSubpathMatches(t *testing.T) {
	// A mount entry must match the mount point exactly: treating a parent or a
	// child mount as covering the path would report a removed mount as present
	// and leave the zombie behind.
	dir := filepath.Join(t.TempDir(), "Qrypt")
	child := filepath.Join(dir, "nested")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	withMountTable(t, "/", child)
	if mountedAt(dir) {
		t.Fatalf("mountedAt(%q) = true for a child mount under it", dir)
	}
}

func TestMountedAtTreatsASymlinkedMountPointAsItsTarget(t *testing.T) {
	// Configs may name the mount point through a symlink; the kernel records
	// the resolved path, so the comparison has to resolve it too.
	target := filepath.Join(t.TempDir(), "real")
	link := filepath.Join(filepath.Dir(target), "link")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}

	withMountTable(t, "/", resolvedTarget)
	if !mountedAt(link) {
		t.Fatalf("mountedAt(%q) = false although its target %q is mounted", link, target)
	}
}
