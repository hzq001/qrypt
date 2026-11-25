package vfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/drivers/localfs"
	idelete "github.com/yinzhenyu/qrypt/pkg/vfs/delete"
)

// deleteTestDelay is short enough to keep the suite fast and long enough for the
// delayed-delete path (the scheduled-and-executed branches) to be exercised.
const deleteTestDelay = 20 * time.Millisecond

// removeSpy counts Remove calls and fails them with err; the real local
// filesystem behind it keeps the rest of the driver behavior honest.
type removeSpy struct {
	drive.Driver
	err   error
	mu    sync.Mutex
	calls int
}

func (d *removeSpy) Remove(ctx context.Context, entry drive.Entry) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	return d.Driver.Remove(ctx, entry)
}

func (d *removeSpy) removeCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func seedRemoteFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("payload"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func waitForDelete(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was not observed before the deadline", what)
}

func closeVFS(t *testing.T, fs *VFS) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fs.Close(ctx); err != nil {
		t.Fatalf("close vfs: %v", err)
	}
}

func startVFS(t *testing.T, fs *VFS) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fs.Start(ctx)
	t.Cleanup(cancel)
	return cancel
}

func recordedDelete(t *testing.T, fs *VFS, path string) FailedRemoteDelete {
	t.Helper()
	for _, failure := range fs.FailedRemoteDeletes() {
		if failure.Path == path {
			return failure
		}
	}
	t.Fatalf("%s is not reported as an uncommitted delete", path)
	return FailedRemoteDelete{}
}

// TestQueuedDeleteIsReportedBeforeItRuns pins the contract a caller depends on:
// from the moment the unlink is accepted, the delete is reported as
// uncommitted, so no caller can claim success the backend has not confirmed.
func TestQueuedDeleteIsReportedBeforeItRuns(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	seedRemoteFile(t, remoteDir, "x.bin")
	fs, err := New(&removeSpy{Driver: localfs.New(remoteDir)}, Options{
		StorageDir: t.TempDir(), UploadDelay: time.Hour, DeleteDelay: time.Hour,
	})
	if err != nil {
		t.Fatalf("new vfs: %v", err)
	}
	defer closeVFS(t, fs)
	startVFS(t, fs)

	if err := fs.Remove(context.Background(), "/x.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	queued := recordedDelete(t, fs, "/x.bin")
	if queued.Attempts != 0 || queued.Error != "" {
		t.Fatalf("queued delete = %+v, want no attempt and no error yet", queued)
	}
	if !queued.Pending {
		t.Fatal("a queued delete must report its pending timer")
	}
	if !fs.deleteVisibility().IsDeleted("/x.bin") {
		t.Fatal("an accepted unlink must keep the path hidden")
	}
}

// TestFailedRemoteDeleteIsReportedAndDurable is the regression pin for the
// silent-success bug: a remote delete that did not commit must be recorded
// durably, reported through the delete inspector, and keep the path hidden.
func TestFailedRemoteDeleteIsReportedAndDurable(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	seedRemoteFile(t, remoteDir, "x.bin")
	storage := t.TempDir()
	driver := &removeSpy{Driver: localfs.New(remoteDir), err: errors.New("baidu api errno 12: file not exist")}
	fs, err := New(driver, Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: deleteTestDelay})
	if err != nil {
		t.Fatalf("new vfs: %v", err)
	}
	if err := fs.Remove(context.Background(), "/x.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	startVFS(t, fs)

	waitForDelete(t, "failed attempt", func() bool { return recordedDelete(t, fs, "/x.bin").Attempts == 1 })
	got := recordedDelete(t, fs, "/x.bin")
	if !strings.Contains(got.Error, "errno 12") {
		t.Fatalf("failure error = %q, want the driver's text", got.Error)
	}
	if got.Pending {
		t.Fatal("a deterministic failure must not report a pending retry")
	}
	if driver.removeCalls() != 1 {
		t.Fatalf("remove calls = %d, want exactly one attempt", driver.removeCalls())
	}
	if !fs.deleteVisibility().IsDeleted("/x.bin") {
		t.Fatal("a failed delete must keep the path hidden until it commits or is cancelled")
	}
	closeVFS(t, fs)

	journal, err := idelete.OpenJournal(storage)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer journal.Close()
	record, ok := journal.Record("/x.bin")
	if !ok {
		t.Fatal("the failed delete was not recorded durably")
	}
	if record.Attempts != 1 || !strings.Contains(record.Error, "errno 12") || record.Name != "x.bin" {
		t.Fatalf("durable record = %+v, want the attempt and the driver's error", record)
	}
}

// TestQueuedDeleteIntentSurvivesRestartAndCompletes pins the replay contract: an
// unlink the process died before running is still known after Start, and the
// re-armed attempt commits the delete the user asked for.
func TestQueuedDeleteIntentSurvivesRestartAndCompletes(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	seedRemoteFile(t, remoteDir, "x.bin")
	storage := t.TempDir()

	first, err := New(localfs.New(remoteDir), Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: time.Hour})
	if err != nil {
		t.Fatalf("new first vfs: %v", err)
	}
	if err := first.Remove(context.Background(), "/x.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	closeVFS(t, first)
	if _, err := os.Stat(filepath.Join(remoteDir, "x.bin")); err != nil {
		t.Fatalf("the delete must not have run yet: %v", err)
	}
	journal, err := idelete.OpenJournal(storage)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	record, ok := journal.Record("/x.bin")
	if !ok || record.Attempts != 0 {
		t.Fatalf("queued intent did not survive the restart: %+v (ok=%t)", record, ok)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	second, err := New(localfs.New(remoteDir), Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: deleteTestDelay})
	if err != nil {
		t.Fatalf("new second vfs: %v", err)
	}
	defer closeVFS(t, second)
	startVFS(t, second)

	waitForDelete(t, "replayed delete", func() bool {
		_, err := os.Stat(filepath.Join(remoteDir, "x.bin"))
		return errors.Is(err, os.ErrNotExist)
	})
	waitForDelete(t, "cleared record", func() bool { return len(second.FailedRemoteDeletes()) == 0 })
}

// TestFailedDeleteStaysReportedAfterRestart covers the other restart outcome: a
// failure that cannot be retried keeps being reported with its attempt count
// instead of vanishing with the process.
func TestFailedDeleteStaysReportedAfterRestart(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	seedRemoteFile(t, remoteDir, "x.bin")
	storage := t.TempDir()
	failure := errors.New("baidu api errno 12: file not exist")

	firstDriver := &removeSpy{Driver: localfs.New(remoteDir), err: failure}
	first, err := New(firstDriver, Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: deleteTestDelay})
	if err != nil {
		t.Fatalf("new first vfs: %v", err)
	}
	if err := first.Remove(context.Background(), "/x.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	startVFS(t, first)
	waitForDelete(t, "failed attempt", func() bool { return recordedDelete(t, first, "/x.bin").Attempts == 1 })
	closeVFS(t, first)

	secondDriver := &removeSpy{Driver: localfs.New(remoteDir), err: failure}
	second, err := New(secondDriver, Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: deleteTestDelay})
	if err != nil {
		t.Fatalf("new second vfs: %v", err)
	}
	defer closeVFS(t, second)
	startVFS(t, second)

	// Replay re-hides the path and keeps reporting the failure; a deterministic
	// failure must not spend another attempt.
	waitForDelete(t, "replayed hidden path", func() bool { return second.deleteVisibility().IsDeleted("/x.bin") })
	failed := second.FailedRemoteDeletes()
	if len(failed) != 1 || failed[0].Attempts != 1 || failed[0].Pending {
		t.Fatalf("failures after restart = %+v, want the recorded failure with its attempt count", failed)
	}
	time.Sleep(5 * deleteTestDelay)
	if calls := secondDriver.removeCalls(); calls != 0 {
		t.Fatalf("a deterministic failure was retried after restart: %d remove calls", calls)
	}
}

// TestTransientDeleteFailureArmsRetry is the transient half of the policy: a
// retryable failure records the attempt and arms the next one, so the record
// keeps reporting the path as uncommitted instead of silently succeeding.
func TestTransientDeleteFailureArmsRetry(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	seedRemoteFile(t, remoteDir, "x.bin")
	driver := &removeSpy{Driver: localfs.New(remoteDir), err: errors.New("network is unreachable")}
	fs, err := New(driver, Options{StorageDir: t.TempDir(), UploadDelay: time.Hour, DeleteDelay: deleteTestDelay})
	if err != nil {
		t.Fatalf("new vfs: %v", err)
	}
	defer closeVFS(t, fs)
	if err := fs.Remove(context.Background(), "/x.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	startVFS(t, fs)

	waitForDelete(t, "failed attempt", func() bool { return recordedDelete(t, fs, "/x.bin").Attempts == 1 })
	// The attempt is journalled before the retry is armed, so wait for the
	// arming itself rather than for the record.
	waitForDelete(t, "armed retry", func() bool { return fs.deletes.deleteState.Scheduled("/x.bin") })
	got := recordedDelete(t, fs, "/x.bin")
	if !strings.Contains(got.Error, "network is unreachable") {
		t.Fatalf("failure = %+v, want the driver's error text", got)
	}
	if !got.Pending {
		t.Fatal("a retryable failure must report the next attempt as pending")
	}
}

// TestAbortedAttemptKeepsTheRecordReplayable pins the shutdown contract: an
// attempt our own teardown cancelled was never answered by the backend, so the
// durable record must stay unanswered - replayable by the next owner - instead
// of reporting a failure the backend never gave.
func TestAbortedAttemptKeepsTheRecordReplayable(t *testing.T) {
	t.Parallel()
	remoteDir := t.TempDir()
	seedRemoteFile(t, remoteDir, "x.bin")
	storage := t.TempDir()
	driver := &removeSpy{Driver: localfs.New(remoteDir), err: context.Canceled}
	fs, err := New(driver, Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: time.Hour})
	if err != nil {
		t.Fatalf("new vfs: %v", err)
	}
	if err := fs.Remove(context.Background(), "/x.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	entries, err := driver.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	entry := drive.Entry{}
	for _, candidate := range entries {
		if candidate.Name == "x.bin" {
			entry = candidate
		}
	}
	if entry.ID == "" {
		t.Fatal("x.bin not listed by the driver")
	}

	// What a shutdown does to a queued attempt: it runs with a cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fs.deleteRemote(ctx, "/x.bin", entry)

	got := recordedDelete(t, fs, "/x.bin")
	if got.Attempts != 0 || got.Error != "" || !got.Pending {
		t.Fatalf("record after an aborted attempt = %+v, want it left unanswered", got)
	}
	if driver.removeCalls() != 1 {
		t.Fatalf("remove calls = %d, want the aborted attempt as the only call", driver.removeCalls())
	}
	if !fs.deleteVisibility().IsDeleted("/x.bin") {
		t.Fatal("an aborted delete must keep the path hidden until it commits or is cancelled")
	}
	closeVFS(t, fs)

	journal, err := idelete.OpenJournal(storage)
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	defer journal.Close()
	record, ok := journal.Record("/x.bin")
	if !ok {
		t.Fatal("the aborted attempt dropped the durable intent")
	}
	if record.Attempts != 0 || record.Error != "" || record.Name != "x.bin" {
		t.Fatalf("durable record = %+v, want no attempt and no error", record)
	}
}

// TestMountWithJournalExposesDeleteInspector documents the configuration the
// whole pipeline depends on: a mount with a storage directory keeps a durable
// delete journal, so delete failures can still be reported after the process is
// gone.
func TestMountWithJournalExposesDeleteInspector(t *testing.T) {
	t.Parallel()
	storage := t.TempDir()
	fs, err := New(localfs.New(t.TempDir()), Options{StorageDir: storage, UploadDelay: time.Hour, DeleteDelay: deleteTestDelay})
	if err != nil {
		t.Fatalf("new vfs: %v", err)
	}
	defer closeVFS(t, fs)
	if fs.deletes.journal == nil {
		t.Fatal("a mount with a storage directory must keep a delete journal")
	}
	if inspector, ok := any(fs).(DeleteInspector); !ok || inspector == nil {
		t.Fatal("the vfs must expose the delete inspector")
	}
	if _, err := os.Stat(filepath.Join(storage, idelete.JournalName)); err != nil {
		t.Fatalf("delete journal not created next to the upload journal: %v", err)
	}
}
