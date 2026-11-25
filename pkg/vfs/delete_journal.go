package vfs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/logging"
	idelete "github.com/yinzhenyu/qrypt/pkg/vfs/delete"
	"github.com/yinzhenyu/qrypt/pkg/vfs/view"
)

// This file owns the durable half of the delete pipeline: the per-mount
// journal that outlives the process (glossary: 延迟删除 / 挂载写入路径的内部记账记录).
//
// A remote delete is the one operation qrypt cannot reconstruct from the
// backend: the user's intent ("this path is gone") only exists locally. The
// journal records that intent before the remote call, keeps the attempt budget
// across restarts, and keeps reporting a failure until the delete commits or
// someone cancels it - so neither a crash nor a backend error can turn a
// failed delete into a silent success.

// recordDeleteIntent persists the intent to delete path before any remote
// attempt runs. A failed write is reported but does not abort the delete: the
// remote operation can still succeed, and losing the record only costs
// durability.
func (v *VFS) recordDeleteIntent(path string, entry drive.Entry) {
	journal := v.deletes.journal
	if journal == nil {
		return
	}
	if err := journal.Put(idelete.RecordFor(path, entry)); err != nil {
		logging.L.Warnf("[VFS] delete journal record failed path=%q id=%q err=%v", path, entry.ID, err)
	}
}

// recordDeleteFailure persists one failed remote attempt and decides whether
// another retry should be armed, leaving visibility transition to the caller
// so active/outstanding state does not drop before the retry timer is armed.
// Deterministic failures (pkg/drive's shared taxonomy: not_found, conflict,
// permission, and the unknown class that carries Baidu's errno 12) are never
// retried - the backend will answer the same way again - so the record stays
// and is reported to whoever asks (DeleteInspector).
func (v *VFS) recordDeleteFailure(path string, err error) (drive.Entry, time.Duration, bool) {
	journal := v.deletes.journal
	if journal == nil {
		return drive.Entry{}, 0, false
	}
	record, ok := journal.Record(path)
	if !ok {
		// No durable intent (the journal is disabled or its write failed):
		// there is nothing to retry and nothing to report later.
		logging.L.Warnf("[VFS] delete failed without a journal record path=%q err=%v", path, err)
		return drive.Entry{}, 0, false
	}
	entry := record.Entry()
	attempts, jerr := journal.RecordAttempt(path, entry, err)
	if jerr != nil {
		logging.L.Warnf("[VFS] delete journal attempt record failed path=%q err=%v", path, jerr)
	}
	delay, retry := idelete.RetryPlan(attempts, err, v.lifecycleContext().Err() == nil)
	if !retry {
		logging.L.Errorf("[VFS] delete remote failed path=%q id=%q dir=%t attempts=%d err=%v", path, entry.ID, entry.IsDir, attempts, err)
		return entry, 0, false
	}
	logging.L.Warnf("[VFS] delete remote failed, retry scheduled path=%q id=%q dir=%t attempts=%d delay=%s err=%v", path, entry.ID, entry.IsDir, attempts, delay, err)
	return entry, delay, true
}

// deleteCommitted drops a path's durable record: the remote delete succeeded,
// or the user cancelled or restored the path.
func (v *VFS) deleteCommitted(path string) {
	journal := v.deletes.journal
	if journal == nil {
		return
	}
	if err := journal.Done(path); err != nil {
		logging.L.Warnf("[VFS] delete journal clear failed path=%q err=%v", path, err)
	}
}

// deleteCommittedUnder drops the durable records of every queued delete inside
// dir: a directory delete or takeover supersedes them, while dir's own record
// (if any) is kept.
func (v *VFS) deleteCommittedUnder(dir string) {
	journal := v.deletes.journal
	if journal == nil {
		return
	}
	if err := journal.DoneUnder(dir); err != nil {
		logging.L.Warnf("[VFS] delete journal clear under failed dir=%q err=%v", dir, err)
	}
}

// armDeleteRetry schedules another remote attempt for a queued delete, keeping
// the recorded failure text so the view keeps explaining why the path is still
// hidden.
func (v *VFS) armDeleteRetry(path string, entry drive.Entry, delay time.Duration) {
	if v.lifecycleContext().Err() != nil {
		v.deleteVisibility().AbortDelete(path)
		return
	}
	newVFSDeleteScheduler(v).ScheduleRetry(path, delay, func() {
		logging.L.Infof("[VFS] delete retry fired path=%q id=%q dir=%t", path, entry.ID, entry.IsDir)
		v.deleteRemote(v.lifecycleContext(), path, entry)
	})
}

// FailedRemoteDeletes implements DeleteInspector: it reports the remote deletes
// this mount recorded but never committed. A record exists from the moment the
// unlink is accepted until the backend confirms the delete, so a caller that
// owns the user's intent can fail loudly instead of reporting success nobody
// observed. A record with no error text is one that has not failed yet (still
// queued, running, or waiting for its next attempt).
func (v *VFS) FailedRemoteDeletes() []FailedRemoteDelete {
	journal := v.deletes.journal
	if journal == nil {
		return nil
	}
	records := journal.Records()
	out := make([]FailedRemoteDelete, 0, len(records))
	for _, record := range records {
		out = append(out, FailedRemoteDelete{
			Path:      record.Path,
			Error:     record.Error,
			Attempts:  record.Attempts,
			Pending:   v.deletes.deleteState.Scheduled(record.Path),
			UpdatedAt: record.UpdatedAt,
		})
	}
	return out
}

// resumeDeletes replays the durable delete journal after Start: every recorded
// path is hidden in the local view again, its failure text is restored, and -
// while the attempt budget is unspent - the next attempt is armed. A record
// that already spent its budget stays as a reported failure; nothing here
// restores a path into the view.
func (v *VFS) resumeDeletes(ctx context.Context) {
	journal := v.deletes.journal
	if journal == nil {
		return
	}
	records := journal.Records()
	if len(records) == 0 {
		return
	}
	vis := v.deleteVisibility()
	armed := 0
	failed := 0
	for _, record := range records {
		entry := record.Entry()
		vis.MarkDeleted(record.Path, entry)
		if record.Error != "" {
			vis.MarkDeleteFailed(record.Path, errors.New(record.Error))
		}
		delay, retry := idelete.ResumePlan(record, ctx.Err() == nil)
		if !retry {
			failed++
			continue
		}
		v.armDeleteRetry(record.Path, entry, delay)
		armed++
	}
	logging.L.Infof("[VFS] delete journal replayed records=%d armed=%d failed=%d", len(records), armed, failed)
}

// deleteVisibility builds the view-facing delete surface (glossary: 延迟删除).
// The overlay and delayed-delete states share one lock, so every compound
// critical section runs in the view package.
func (v *VFS) deleteVisibility() view.Visibility {
	return view.NewVisibility(v.view.Overlay(), v.deletes.deleteState, v.view, nil)
}

// openDeleteJournal opens the mount's durable delete journal next to the upload
// journal (pending.jsonl) in the mount's storage directory, so both journals
// share one directory and one lifecycle. A VFS built without a storage
// directory keeps no durable delete record: that is reported once here instead
// of degrading silently on every delete.
func openDeleteJournal(uploadDir string) (*idelete.Journal, error) {
	if uploadDir == "" {
		logging.L.Warnf("[VFS] no storage directory configured: remote delete failures will not be recorded durably")
		return nil, nil
	}
	journal, err := idelete.OpenJournal(uploadDir)
	if err != nil {
		return nil, fmt.Errorf("vfs: %w", err)
	}
	return journal, nil
}
