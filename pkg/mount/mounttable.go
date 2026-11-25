//go:build !windows

package mount

import (
	"os"
	"path/filepath"
	"strings"
)

// mountTableHas reports whether path is a registered mount point, using the
// platform mount table instead of stat(2).
//
// stat(2) is the wrong probe for this question. A FUSE-T mount whose server
// side has died still answers stat(2) on its root inode, and any operation that
// reaches the dead NFS peer blocks for seconds before failing; either way the
// answer says "the mount is unusable", not "the mount is registered". The mount
// table answers exactly what the caller asks, without touching the mount.
//
// It never reports "/" or the user's home directory, so stale-mount cleanup can
// never unmount a system volume or the home directory by mistake.
func mountTableHas(path string) bool {
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	if abs == "/" {
		return false
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && abs == filepath.Clean(home) {
		return false
	}
	// The kernel records the resolved path, so a mount point configured
	// through a symlink (/var -> /private/var on macOS, a link in the home
	// directory) would otherwise never match. A miss here reads as "already
	// unmounted", which is the dangerous direction: it would leave a live
	// mount behind instead of restarting it.
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	return mountTableContains(abs)
}

// splitMountTableLine returns the mount point of one mount-table entry.
func splitMountTableLine(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", false
	}
	return unescapeMountField(fields[1]), true
}

// unescapeMountField decodes the octal escapes (\040 space, \011 tab, \012
// newline, \134 backslash) the kernel writes into mount-table entries.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var sb strings.Builder
	sb.Grow(len(field))
	for i := 0; i < len(field); i++ {
		if field[i] != '\\' || i+3 >= len(field) {
			sb.WriteByte(field[i])
			continue
		}
		value := 0
		valid := true
		for _, digit := range field[i+1 : i+4] {
			if digit < '0' || digit > '7' {
				valid = false
				break
			}
			value = value*8 + int(digit-'0')
		}
		if !valid {
			sb.WriteByte(field[i])
			continue
		}
		sb.WriteByte(byte(value))
		i += 3
	}
	return sb.String()
}
