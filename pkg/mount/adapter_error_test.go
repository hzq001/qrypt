package mount

import (
	"fmt"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"github.com/yinzhenyu/qrypt/pkg/vfs"
)

func TestFuseErrMapsDirectorySentinels(t *testing.T) {
	t.Parallel()
	if got := fuseErr(fmt.Errorf("write: %w", vfs.ErrIsDir)); got != -fuse.EISDIR {
		t.Fatalf("fuseErr(ErrIsDir) = %d, want EISDIR", got)
	}
	if got := fuseErr(fmt.Errorf("rmdir: %w", vfs.ErrDirNotEmpty)); got != -fuse.ENOTEMPTY {
		t.Fatalf("fuseErr(ErrDirNotEmpty) = %d, want ENOTEMPTY", got)
	}
}

func TestSlowOpThresholdDefault(t *testing.T) {
	t.Parallel()
	if got := slowOpThreshold; got != 2*time.Second {
		t.Fatalf("slowOpThreshold = %v, want 2s", got)
	}
}
