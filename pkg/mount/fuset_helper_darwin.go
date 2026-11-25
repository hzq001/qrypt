//go:build darwin

package mount

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// fuseTHelperCommandName is the process FUSE-T runs per mount, including mounts
// using the SMB and FSKit backends. Its historical name does not imply NFS.
const fuseTHelperCommandName = "go-nfsv4"

// fuseTHelperAlive reports whether the FUSE-T helper serving mountPoint is still
// running.
//
// FUSE-T forks one helper per mount and passes the mount point as its last
// argument ("go-nfsv4 --volname Qrypt /Users/me/Qrypt"), so the mount point
// identifies the helper. When it is gone the mount has no server and cannot
// recover; the kernel keeps the entry and every access fails or hangs. This is
// the only reliable liveness signal for FUSE-T: its dead mounts answer a probe
// with EIO, which is indistinguishable from a live mount whose backend call
// failed.
//
// An unreadable process table reports "alive": treating a mount as dead when
// the answer is unknown would detach a working filesystem, which is worse than
// leaving the user's existing failure in place.
var fuseTHelperAliveProbe = fuseTHelperAlive

func fuseTHelperAlive(mountPoint string) bool {
	// FUSE-T is handed the resolved path, and macOS makes /var a symlink to
	// /private/var, so compare on the resolved form.
	if resolved, err := filepath.EvalSymlinks(mountPoint); err == nil {
		mountPoint = resolved
	}
	output, err := exec.Command("ps", "-ax", "-o", "command=").Output()
	if err != nil {
		return true
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		if !strings.Contains(line, fuseTHelperCommandName) {
			continue
		}
		// ps prints arguments without quoting: splitting on whitespace would
		// break mount points containing spaces. The final argument is the full
		// resolved mount point, preceded by its argument separator.
		if strings.HasSuffix(line, " "+mountPoint) {
			return true
		}
	}
	return false
}
