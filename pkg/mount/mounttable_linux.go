//go:build !windows && !darwin

package mount

import (
	"bufio"
	"os"
)

// mountTableContains reports whether abs is in the kernel mount table via
// /proc/self/mounts. That file is kernel-maintained data read without touching
// any mounted filesystem, so it answers immediately even when the mount at abs
// has a dead server behind it.
func mountTableContains(abs string) bool {
	file, err := os.Open("/proc/self/mounts")
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	// Entries are short; the kernel escapes special bytes into octal
	// sequences, so allow some headroom without unbounded growth.
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		if mountPoint, ok := splitMountTableLine(scanner.Text()); ok && mountPoint == abs {
			return true
		}
	}
	return false
}
