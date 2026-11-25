//go:build windows

package mount

import "os/exec"

func unmountCommand(mountPoint string) *exec.Cmd {
	// WinFSP handles unmounting via the FUSE API (host.Unmount()).
	// The exec fallback is a best-effort attempt using the mount
	// manager utility.
	return exec.Command("net", "use", mountPoint, "/delete")
}

// mountedAt keeps the Windows fallback unconditional: WinFSP owns the mount
// point, so the device comparison used on unix does not apply.
func mountedAt(string) bool {
	return true
}

func forceUnmount(mountPoint string) error {
	return unmountCommand(mountPoint).Run()
}
