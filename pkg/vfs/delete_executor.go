package vfs

import (
	"context"
	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/logging"
	idelete "github.com/yinzhenyu/qrypt/pkg/vfs/delete"
	"github.com/yinzhenyu/qrypt/pkg/vfs/upload"
	"github.com/yinzhenyu/qrypt/pkg/vfs/view"
	"time"
)

func (v *VFS) scheduleDelete(path string, entry drive.Entry) {
	// The intent is durable before the delete is armed: a process that dies
	// between the unlink and the remote call must still know the path was
	// recorded as gone.
	v.recordDeleteIntent(path, entry)
	if v.deletes.delay <= 0 {
		logging.L.Infof("[VFS] delete remote now path=%q id=%q dir=%t", path, entry.ID, entry.IsDir)
		v.deleteRemote(v.lifecycleContext(), path, entry)
		return
	}
	newVFSDeleteScheduler(v).Schedule(path, entry, v.deletes.delay, func() {
		logging.L.Infof("[VFS] delete timer fired path=%q id=%q dir=%t", path, entry.ID, entry.IsDir)
		v.deleteRemote(v.lifecycleContext(), path, entry)
	})
}

// lifecycleContext returns the VFS lifecycle context when Start has run, or
// context.Background() for the short window before Start (deletes scheduled
// during construction in tests). Background tasks derive from it so a VFS
// shutdown cancels in-flight remote operations.
func (v *VFS) lifecycleContext() context.Context {
	if p := v.ctx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

func (v *VFS) deleteRemote(ctx context.Context, path string, entry drive.Entry) {
	idelete.NewExecutor(newVFSDeleteExecutorDeps(v)).Execute(ctx, path, entry)
}

// newVFSDeleteExecutorDeps adapts VFS internals to idelete.ExecutorDeps.
func newVFSDeleteExecutorDeps(v *VFS) idelete.ExecutorDeps {
	return idelete.ExecutorDeps{
		Driver:                   newVFSDriverRuntime(v.driver, v.testEnabled),
		Overlay:                  newVFSDeleteOverlayOps(v),
		Health:                   v.healthTracker,
		Upload:                   newVFSDeleteUploadCleanup(v.uploads),
		WaitForDescendantDeletes: v.waitForActiveChildDeletes,
	}
}

// vfsDeleteOverlayOps adapts the overlay + delayed-delete domain to
// idelete.OverlayOps (glossary: 延迟删除). The two states share one lock
// (view.NewOverlayDelayedDeleteState), so every compound critical section runs
// in the view package; this adapter is a thin delegation over the exported
// sync surface.
//
// The adapter also owns durability: the journal lives here rather than in
// pkg/vfs/delete, because a remote attempt's outcome is only durable once the
// VFS has decided what the failure means (retry vs. report) - see delete_journal.go.
type vfsDeleteOverlayOps struct {
	v   *VFS
	vis view.Visibility
}

func newVFSDeleteOverlayOps(v *VFS) vfsDeleteOverlayOps {
	return vfsDeleteOverlayOps{v: v, vis: v.deleteVisibility()}
}

func (r vfsDeleteOverlayOps) BeginDelete(path string, entryID string) bool {
	return r.vis.BeginDelete(path, entryID)
}

func (r vfsDeleteOverlayOps) MarkDeleteActive(path string, entry drive.Entry) {
	r.vis.MarkDeleteActive(path, entry)
}

func (r vfsDeleteOverlayOps) MarkDeleteFailed(path string, err error) {
	entry, delay, retry := r.v.recordDeleteFailure(path, err)
	if !retry {
		r.vis.MarkDeleteFailed(path, err)
		return
	}
	r.vis.MarkDeleteFailedRetry(path, err)
	r.v.armDeleteRetry(path, entry, delay)
}

// MarkDeleteAborted clears the in-flight attempt without a durable failure: the
// remote was never given the chance to answer, so the recorded intent stays
// unanswered for the next replay instead of being reported as failed.
func (r vfsDeleteOverlayOps) MarkDeleteAborted(path string) {
	r.vis.AbortDelete(path)
}

func (r vfsDeleteOverlayOps) MarkDeleteComplete(path string, entry drive.Entry) {
	r.vis.MarkDeleteComplete(path, entry)
	r.v.deleteCommitted(path)
}

func (r vfsDeleteOverlayOps) CancelDelete(path string) {
	r.vis.CancelDelete(path)
	r.v.deleteCommitted(path)
}

type vfsDeleteUploadCleanup struct {
	uploads *upload.Domain
}

func newVFSDeleteUploadCleanup(uploads *upload.Domain) vfsDeleteUploadCleanup {
	return vfsDeleteUploadCleanup{uploads: uploads}
}

func (r vfsDeleteUploadCleanup) RemoveUploadState(path string) {
	r.uploads.RemoveAfterRemoteDelete(path)
}

type vfsDeleteScheduler struct {
	v           *VFS
	vis         view.Visibility
	deleteState *view.DelayedDeleteState
}

func newVFSDeleteScheduler(v *VFS) vfsDeleteScheduler {
	return vfsDeleteScheduler{
		v:           v,
		vis:         v.deleteVisibility(),
		deleteState: v.deletes.deleteState,
	}
}

func (s vfsDeleteScheduler) Schedule(path string, entry drive.Entry, delay time.Duration, fire func()) {
	s.vis.ScheduleDelete(path, delay, fire)
}

// ScheduleRetry arms another attempt for a delete that already failed.
// The recorded failure stays in the view until the attempt succeeds.
func (s vfsDeleteScheduler) ScheduleRetry(path string, delay time.Duration, fire func()) {
	s.vis.ScheduleDeleteRetry(path, delay, fire)
}

// TakeoverDirectory cancels the pending deletes of a directory's children: the
// directory's own delete (or restore) supersedes them, so their durable records
// are dropped too.
func (s vfsDeleteScheduler) TakeoverDirectory(dir string) {
	s.vis.TakeoverDirectory(dir)
	s.v.deleteCommittedUnder(dir)
}

func (s vfsDeleteScheduler) CancelChildren(dir string) {
	s.vis.CancelChildren(dir)
	s.v.deleteCommittedUnder(dir)
}

func (s vfsDeleteScheduler) StopAll() {
	s.deleteState.StopAll()
}

func (v *VFS) waitForActiveChildDeletes(ctx context.Context, dir string) error {
	return v.deletes.deleteState.WaitActiveChildren(ctx, dir)
}
