package mount

import (
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"github.com/yinzhenyu/qrypt/pkg/drive"
)

// statFor fills a fresh Stat_t the way Getattr does.
func statFor(entry drive.Entry, path string) *fuse.Stat_t {
	stat := &fuse.Stat_t{}
	fillStat(stat, entry, path)
	return stat
}

// A path whose entry carries no modtime must still report the same mtime on
// every GETATTR. Synthesizing time.Now() per call turns the path into a
// standing invalidation generator: the NFS client sees a changed attribute on
// every probe and re-runs nfs_vinvalbuf over the vnode, which is the window
// the macOS NFS kext panics in (docs/adr/0006-served-attributes-are-stable.md).
func TestFillStatZeroModTimeIsStableAcrossCalls(t *testing.T) {
	t.Parallel()
	entry := drive.Entry{ID: "virtual-root-apple-metadata:.DS_Store", Name: ".DS_Store", IsDir: true}

	first := statFor(entry, "/.DS_Store")
	time.Sleep(2 * time.Millisecond)
	second := statFor(entry, "/.DS_Store")

	if first.Mtim != second.Mtim || first.Ctim != second.Ctim || first.Atim != second.Atim {
		t.Fatalf("zero-modtime entry reported changing timestamps: first=%v second=%v", first.Mtim, second.Mtim)
	}
	if first.Birthtim != second.Birthtim {
		t.Fatalf("zero-modtime entry reported changing birthtime: first=%v second=%v", first.Birthtim, second.Birthtim)
	}
}

// The same virtual path must keep one inode while a file is being written:
// a pending upload is keyed by a staging generation ID, so hashing that ID
// renumbers the inode on every write generation and again when the upload
// completes and the entry switches to the remote ID.
func TestStableInodeKeepsPathIdentityAcrossGenerations(t *testing.T) {
	t.Parallel()
	first := drive.Entry{ID: "file.txt-dmb88pq1lha8", Name: "file.txt", Size: 10}
	second := drive.Entry{ID: "file.txt-dmb88s2gmnpc", Name: "file.txt", Size: 30}
	uploaded := drive.Entry{ID: "/root/file.txt", Name: "file.txt", Size: 30}

	ino := stableInode(first, "/mount/file.txt")
	if got := stableInode(second, "/mount/file.txt"); got != ino {
		t.Fatalf("inode changed between upload generations: %d -> %d", ino, got)
	}
	if got := stableInode(uploaded, "/mount/file.txt"); got != ino {
		t.Fatalf("inode changed when the upload completed: %d -> %d", ino, got)
	}
}
