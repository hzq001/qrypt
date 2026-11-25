package mount

import (
	"context"
	"time"
)

const metadataOpTimeout = 10 * time.Second

// beginMetadataOp bounds the complete read-only metadata lookup, including
// waiting for an existing directory prefetch. FUSE-T forwards these calls over
// NFS, where leaving a request unanswered can mark the whole volume offline.
// Cancellation is cooperative: the callback still waits for the filesystem
// to return, so no detached operation can outlive its FUSE reply. Mutations and
// file transfers keep their original contexts.
func (a *adapter) beginMetadataOp(op, path string) (context.Context, func(), bool) {
	ctx, done, ok := a.beginOp(op, path)
	if !ok {
		return ctx, done, false
	}
	ctx, cancel := context.WithTimeout(ctx, metadataOpTimeout)
	return ctx, func() {
		cancel()
		done()
	}, true
}
