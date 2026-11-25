//go:build darwin

package mount

import (
	"errors"
	"syscall"
)

// isStaleMountError reports whether a stat(2) error on the mount point means the
// directory is hidden behind a macFUSE mount whose daemon is gone.
//
// ENXIO ("device not configured") is what a dead macFUSE mount reports for the
// operations that do reach the kernel's mount layer, and EIO covers a volume the
// kernel dropped; ENOTCONN/ENODEV are kept for parity with Linux FUSE. A false
// positive only costs one detach of a mount whose stat already failed.
func isStaleMountError(err error) bool {
	return errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.EIO) ||
		errors.Is(err, syscall.ENXIO)
}

// isDeadDaemonError reports whether an error from an operation that reached the
// daemon proves the daemon is gone. It is deliberately narrower than
// isStaleMountError: a live qrypt mount answers EIO when a backend call fails,
// and detaching a mount that is merely reporting a failed request would make the
// user's drive disappear. Only "the volume is not there" errnos count.
func isDeadDaemonError(err error) bool {
	return errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENXIO)
}

// staleMountDetachers lists the unmount tools to try, in order. macFUSE ships no
// fusermount; diskutil force-unmounts a volume that umount refuses.
func staleMountDetachers(mountPoint string) []detachCandidate {
	return []detachCandidate{
		{name: "umount", args: []string{"-f", mountPoint}},
		{name: "diskutil", args: []string{"unmount", "force", mountPoint}},
	}
}

// detachHint names the command that clears a dead mount on macOS, for error
// messages that cannot detach it themselves.
func detachHint(mountPoint string) string {
	return "diskutil unmount force " + mountPoint
}

// statfsMount reports the canonical root, filesystem type and mount source of
// the mount that covers path. macOS answers statfs(2) for a dead FUSE mount as
// well -- unlike stat(2) it does not go through the daemon -- which is what
// makes the mount root discoverable before anything is detached.
//
// The source ("fuse-t:/Qrypt", "/dev/disk3s5") is needed on top of the type
// because FUSE-T exports the filesystem as a localhost NFS mount: its type is
// plain "nfs", indistinguishable from a network NFS mount, while the source
// still names FUSE-T.
func statfsMount(path string) (root, fsType, source string, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", "", "", false
	}
	return cString(st.Mntonname[:]), cString(st.Fstypename[:]), cString(st.Mntfromname[:]), true
}

// cString trims the NUL padding of a C char array, as returned in
// syscall.Statfs_t.
func cString(field []int8) string {
	buf := make([]byte, 0, len(field))
	for _, c := range field {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}
