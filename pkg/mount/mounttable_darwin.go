//go:build darwin

package mount

import "syscall"

// mntNoWait asks getfsstat(2) for the cached mount table; syscall does not
// export the flag on darwin.
const mntNoWait = 2

// mountTableContains reports whether abs is in the kernel mount table.
// getfsstat(2) with MNT_NOWAIT answers from the cached table without touching
// any mounted filesystem, so it stays fast even when the mount at abs has a
// dead server behind it.
func mountTableContains(abs string) bool {
	for _, mountPoint := range mountTableMountPoints() {
		if mountPoint == abs {
			return true
		}
	}
	return false
}

// mountTableMountPoints reads every mount point from the kernel mount table.
// It is the seam the tests replace, so mount-table logic can be exercised
// without mounting anything.
var mountTableMountPoints = readMountTableMountPoints

func readMountTableMountPoints() []string {
	count, err := syscall.Getfsstat(nil, mntNoWait)
	if err != nil || count <= 0 {
		return nil
	}
	// The table can grow between the sizing call and the read; the spare
	// capacity absorbs that without a retry loop.
	buf := make([]syscall.Statfs_t, count+8)
	count, err = syscall.Getfsstat(buf, mntNoWait)
	if err != nil || count <= 0 {
		return nil
	}
	mountPoints := make([]string, 0, count)
	for i := range count {
		mountPoints = append(mountPoints, cString(buf[i].Mntonname[:]))
	}
	return mountPoints
}
