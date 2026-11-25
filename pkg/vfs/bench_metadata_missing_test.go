package vfs

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/vfs/listing"
	"github.com/yinzhenyu/qrypt/pkg/vfs/view"
)

func BenchmarkVFSStatMissingWithFreshParent(b *testing.B) {
	for _, cached := range []int{0, 1000, 10000, 100000} {
		b.Run(strconv.Itoa(cached), func(b *testing.B) {
			drv := drive.NewFakeDriver()
			if err := drv.Seed(map[string]string{"dir/exists.txt": "data"}); err != nil {
				b.Fatal(err)
			}
			fs, err := New(drv, Options{StorageDir: b.TempDir()})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = fs.Close(context.Background()) })
			ctx := listing.WithoutDirPrefetch(context.Background())
			if _, err := fs.List(ctx, "/dir"); err != nil {
				b.Fatal(err)
			}
			entries := make([]drive.Entry, cached)
			for i := range entries {
				name := "file-" + strconv.Itoa(i)
				entries[i] = drive.Entry{ID: name, Name: name}
			}
			view.NewRuntime(fs.view).CacheListedChildren("/unrelated", entries)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := fs.Stat(ctx, "/dir/.git"); !errors.Is(err, ErrNotFound) {
					b.Fatalf("Stat = %v, want ErrNotFound", err)
				}
			}
		})
	}
}
