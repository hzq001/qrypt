//go:build darwin

package mount

import (
	"testing"
)

const testMountPoint = "/Users/example/Qrypt"

// withFuseTMount describes a FUSE-T mount (type "nfs", source "fuse-t:...") at
// testMountPoint and returns a restore function.
func withFuseTMount(t *testing.T, helperAlive bool) {
	t.Helper()
	previousStatfs := statfsMountProbe
	statfsMountProbe = func(string) (string, string, string, bool) {
		return testMountPoint, "nfs", "fuse-t:/Qrypt", true
	}
	t.Cleanup(func() { statfsMountProbe = previousStatfs })

	previousTable := mountTableMountPoints
	mountTableMountPoints = func() []string { return []string{"/", testMountPoint} }
	t.Cleanup(func() { mountTableMountPoints = previousTable })

	previousProbe := fuseTHelperAliveProbe
	fuseTHelperAliveProbe = func(string) bool { return helperAlive }
	t.Cleanup(func() { fuseTHelperAliveProbe = previousProbe })
}

func TestFuseTMountIsRecognisedByItsSource(t *testing.T) {
	t.Parallel()
	// FUSE-T exports a localhost NFS mount, so statfs(2) reports the type "nfs"
	// with the source "fuse-t:/Qrypt". Matching only the type would classify it
	// as a network NFS mount, and stale-mount recovery would never see it.
	if got := isFuseFilesystem("nfs", "fuse-t:/Qrypt"); !got {
		t.Fatal("a FUSE-T mount was not recognised as a FUSE filesystem")
	}
	if isFuseTMount("server:/export/share") {
		t.Fatal("a network NFS source was reported as FUSE-T")
	}
	if got := isFuseFilesystem("nfs", "server:/export/share"); got {
		t.Fatal("a network NFS mount was reported as a FUSE filesystem")
	}
}

func TestOrphanedFuseTMountIsReportedDead(t *testing.T) {
	// The mount is still registered but its helper is gone: nothing can answer,
	// so ensureMountPoint has to detach it before the next mount.
	withFuseTMount(t, false)

	if !mountedAt(testMountPoint) {
		t.Fatal("mountedAt reported the FUSE-T mount as gone")
	}
	root, dead := deadFuseMountRoot(testMountPoint)
	if !dead {
		t.Fatal("orphaned FUSE-T mount was not reported as dead")
	}
	if root != testMountPoint {
		t.Fatalf("deadFuseMountRoot root = %q, want %q", root, testMountPoint)
	}
}

func TestFuseTMountWithLiveHelperIsLeftAlone(t *testing.T) {
	// A live mount answers EIO when a backend call fails. Treating that as death
	// would detach a working filesystem, which is worse than the failure the
	// user already has.
	withFuseTMount(t, true)

	if root, dead := deadFuseMountRoot(testMountPoint); dead {
		t.Fatalf("live FUSE-T mount reported as the dead mount rooted at %q", root)
	}
}

func TestFSKitMountUsesItsOwnFuseTHelper(t *testing.T) {
	for _, helperAlive := range []bool{true, false} {
		withFuseTMount(t, helperAlive)
		statfsMountProbe = func(string) (string, string, string, bool) {
			return testMountPoint, "fuse-t", "file:///private/tmp/fuset-session-123/session.json", true
		}
		root, fuseT, ok := fuseMountInfo(testMountPoint)
		if !ok || !fuseT || root != testMountPoint {
			t.Fatalf("FSKit mount info = (%q, %v, %v), want its FUSE-T identity", root, fuseT, ok)
		}
		_, dead := deadFuseMountRoot(testMountPoint)
		if dead == helperAlive {
			t.Fatalf("helperAlive=%v: dead=%v", helperAlive, dead)
		}
	}
}

func TestFuseTHelperProbeUsesMountRootForChildPath(t *testing.T) {
	withFuseTMount(t, true)
	fuseTHelperAliveProbe = func(got string) bool {
		if got != testMountPoint {
			t.Errorf("helper probe path = %q, want mount root %q", got, testMountPoint)
		}
		return got == testMountPoint
	}
	if root, dead := deadFuseMountRoot(testMountPoint + "/local/docs"); dead {
		t.Fatalf("live mount %q was reported dead for a child path", root)
	}
}
