//go:build !windows

package mount

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountedAtRejectsRootAndHome(t *testing.T) {
	t.Parallel()
	if mountedAt("/") {
		t.Fatal(`mountedAt("/") must be false: stale-mount cleanup must never unmount the root volume`)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if mountedAt(home) {
		t.Fatalf("mountedAt(%q) must be false: stale-mount cleanup must never unmount the home directory", home)
	}
}

func TestMountedAtRejectsPlainDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "mnt")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if mountedAt(nested) {
		t.Fatalf("mountedAt(%q) = true for a plain directory", nested)
	}
	if mountedAt(filepath.Join(dir, "missing")) {
		t.Fatal("mountedAt = true for a missing path")
	}
}

func TestForceUnmountLeavesUnmountedPathAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := forceUnmount(dir); err != nil {
		t.Fatalf("forceUnmount(%q) = %v, want nil", dir, err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("forceUnmount removed a directory that was not a mount point: %v", err)
	}
}

func TestUnescapeMountFieldDecodesKernelEscapes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/plain/path":        "/plain/path",
		`/space\040dir`:      "/space dir",
		`/tab\011dir`:        "/tab\tdir",
		`/back\134slash`:     `/back\slash`,
		`/mixed\040\134\040`: "/mixed \\ ",
		`/trailing\04`:       `/trailing\04`, // truncated escape: kept verbatim
		`/invalid\999`:       `/invalid\999`, // out-of-range octal: kept verbatim
		`/文字\040path`:        "/文字 path",
	}
	for in, want := range cases {
		if got := unescapeMountField(in); got != want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitMountTableLineReadsMountPoint(t *testing.T) {
	t.Parallel()
	line := `fuse-t:/Qrypt /Users/me/Qrypt nfs rw,nodev 0 0`
	got, ok := splitMountTableLine(line)
	if !ok || got != "/Users/me/Qrypt" {
		t.Fatalf("splitMountTableLine = (%q, %v), want (/Users/me/Qrypt, true)", got, ok)
	}
	if _, ok := splitMountTableLine("onlyone"); ok {
		t.Fatal("splitMountTableLine accepted a line without a mount point")
	}
}
