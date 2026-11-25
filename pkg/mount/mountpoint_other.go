//go:build !darwin

package mount

// statfsMount reports no mount information. Only macOS needs it:
// deadFuseMountRoot exists because macOS serves stat(2) out of a dead mount's
// root inode, and statfs(2) is how that dead mount is still recognisable. Every
// other platform reaches a dead mount through the stat(2) errno instead (see
// ensureMountPoint).
func statfsMount(string) (root, fsType, source string, ok bool) {
	return "", "", "", false
}

// isDeadDaemonError is only asked by the macOS probe (deadFuseMountRoot), which
// never runs where fuseMountRoot cannot report a mount; keep it aligned with the
// stat(2) classification those platforms use.
func isDeadDaemonError(err error) bool {
	return isStaleMountError(err)
}

// fuseTHelperAliveProbe answers the FUSE-T branch of deadFuseMountRoot. FUSE-T
// only exists on macOS, and statfsMount never reports a mount here, so the
// branch is unreachable off darwin. The sound answer stays "alive": a caller
// that somehow reached it would detach nothing rather than a live mount.
func fuseTHelperAliveProbe(string) bool {
	return true
}
