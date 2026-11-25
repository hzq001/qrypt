//go:build !windows

package mount

import (
	"fmt"
	"os/exec"
	"runtime"
)

func unmountCommand(mountPoint string) *exec.Cmd {
	return exec.Command("umount", "-f", mountPoint)
}

// mountedAt reports whether mountPoint is registered as a mount point.
//
// It reads the mount table instead of calling stat(2) on the path. On a FUSE-T
// mount whose server side has just died, stat(2) blocks until the kernel gives
// up on the dead NFS peer -- several seconds -- and the answer only says "the
// mount is unusable", not whether it is still registered. The mount table
// answers the question the caller asks ("did something unmount this?")
// immediately and without touching the mount.
func mountedAt(mountPoint string) bool {
	return mountTableHas(mountPoint)
}

// forceUnmount releases mountPoint even when nothing answers behind it.
// "umount -f" alone is not enough for a dead FUSE-T session: the kernel keeps
// waiting on the broken NFS connection and refuses to drop the export, so fall
// back to the platform's forced unmount and verify that the mount is gone.
func forceUnmount(mountPoint string) error {
	if !mountedAt(mountPoint) {
		return nil
	}
	umountErr := unmountCommand(mountPoint).Run()
	if !mountedAt(mountPoint) {
		return nil
	}
	fallbackErr := forceUnmountFallback(mountPoint).Run()
	if !mountedAt(mountPoint) {
		return nil
	}
	return fmt.Errorf("mount: %s is still mounted after forced unmount (umount: %v, fallback: %v)", mountPoint, umountErr, fallbackErr)
}

// forceUnmountFallback is the platform-specific eviction used when "umount -f"
// cannot drop a hung mount.
func forceUnmountFallback(mountPoint string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("diskutil", "unmount", "force", mountPoint)
	}
	return exec.Command("umount", "-l", mountPoint)
}
