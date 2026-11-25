//go:build !windows

package mount

import "testing"

// TestMountTableContainsFindsTheRootVolume checks the reader against data the
// kernel definitely has: "/" is always mounted. A reader that silently always
// returned false would make every external-unmount check report "still
// registered" and every forceUnmount skip its work.
func TestMountTableContainsFindsTheRootVolume(t *testing.T) {
	t.Parallel()
	if !mountTableContains("/") {
		t.Fatal(`mountTableContains("/") = false: the root volume is always in the mount table`)
	}
	if mountTableContains("/definitely/not/a/mount/point") {
		t.Fatal("mountTableContains reported an unmounted path as mounted")
	}
}
