// Package delete carries the durable journal of queued remote deletes.
//
// A remote delete is the only operation qrypt cannot replay from the backend:
// the user's intent (this path is gone) must survive a process restart, and a
// failure must stay visible until the delete commits or someone cancels it.
// The journal is the disk half of that intent; pkg/vfs replays it at mount
// start to re-hide the path in the view and schedule the next attempt.
package delete

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
	"github.com/yinzhenyu/qrypt/pkg/logging"
)

// JournalName is the file a mount's queued deletes are recorded in.
const JournalName = "deletes.jsonl"

// Journal compaction watermarks. They are defaults: tests may lower them by
// constructing a journal through openJournal.
const (
	journalCompactMinEntries = 512
	journalCompactFactor     = 4
)

// JournalRecord is one queued or failed remote delete. Attempts counts the
// remote attempts that already ran, so a replay after a restart continues the
// same budget instead of restarting it.
//
// The record carries enough of the drive entry to address the remote object
// again after a restart: its id, name, kind and the remote name an encrypted
// mount derives from it. Driver-private metadata (drive.EntryExtra) is not
// persisted - only the remote name matters for addressing.
type JournalRecord struct {
	Path       string    `json:"path"`
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	IsDir      bool      `json:"is_dir"`
	Size       int64     `json:"size,omitempty"`
	RemoteName string    `json:"remote_name,omitempty"`
	Attempts   int       `json:"attempts"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RecordFor builds the journal record for a queued delete of entry.
func RecordFor(path string, entry drive.Entry) JournalRecord {
	rec := JournalRecord{
		Path:  path,
		ID:    entry.ID,
		Name:  entry.Name,
		IsDir: entry.IsDir,
		Size:  entry.Size,
	}
	if remote, ok := drive.EntryRemoteName(entry); ok {
		rec.RemoteName = remote
	}
	return rec
}

// Entry rebuilds the drive entry a replayed delete addresses. On an encrypted
// mount the remote name is the only extra data the delete path needs; anything
// a driver derived from its own metadata has to be re-fetched by the driver.
func (r JournalRecord) Entry() drive.Entry {
	entry := drive.Entry{ID: r.ID, Name: r.Name, IsDir: r.IsDir, Size: r.Size}
	if r.RemoteName != "" {
		entry.Extra = drive.EntryExtraWrapper{RemoteName: r.RemoteName}
	}
	return entry
}

// journalEntry is one line of the append-only log. ops: "put" upserts a
// record, "done" drops one (the delete committed or was cancelled).
type journalEntry struct {
	Op     string         `json:"op"`
	Record *JournalRecord `json:"record,omitempty"`
	Path   string         `json:"path,omitempty"`
}

// Journal is an append-only log of queued remote deletes, compacted when the
// log grows past a multiple of the live record count. Every append is fsynced:
// losing the intent is exactly what this type exists to prevent.
//
// The zero value is not usable; open one with OpenJournal.
type Journal struct {
	dir  string
	path string

	mu         sync.Mutex
	records    map[string]JournalRecord
	entries    int
	compactAt  int
	file       *os.File
	persistErr error
	durability bool
}

// OpenJournal opens (creating if needed) the journal directory and replays it.
// The directory is created with 0700: it records the user's filesystem intents.
func OpenJournal(dir string) (*Journal, error) {
	return openJournal(dir, journalCompactMinEntries)
}

func openJournal(dir string, compactMinEntries int) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("delete: journal directory required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("delete: create journal directory %s: %w", dir, err)
	}
	j := &Journal{
		dir:        dir,
		path:       filepath.Join(dir, JournalName),
		records:    map[string]JournalRecord{},
		compactAt:  compactMinEntries,
		durability: true,
	}
	if _, err := j.load(); err != nil {
		return nil, err
	}
	return j, nil
}

// Path returns the journal file path.
func (j *Journal) Path() string { return j.path }

// Records returns the live records sorted by path.
func (j *Journal) Records() []JournalRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.recordsLocked()
}

// recordsLocked is the sorted live set. Callers hold the journal lock;
// compaction runs under it and must not re-enter the public accessor.
func (j *Journal) recordsLocked() []JournalRecord {
	out := make([]JournalRecord, 0, len(j.records))
	for _, rec := range j.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}

// Record returns one path's record.
func (j *Journal) Record(path string) (JournalRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec, ok := j.records[path]
	return rec, ok
}

// Put upserts the intent to delete path. Scheduling the same path twice keeps
// the attempt budget already spent.
func (j *Journal) Put(rec JournalRecord) error {
	if rec.Path == "" {
		return errors.New("delete: journal record needs a path")
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if current, ok := j.records[rec.Path]; ok && current.Attempts > rec.Attempts {
		rec.Attempts = current.Attempts
	}
	j.records[rec.Path] = rec
	return j.appendLocked(journalEntry{Op: "put", Record: &rec})
}

// RecordAttempt stores a failed attempt: it increments the attempt count,
// records the driver's error text, and returns the new count.
func (j *Journal) RecordAttempt(path string, entry drive.Entry, err error) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec := RecordFor(path, entry)
	if current, ok := j.records[path]; ok {
		rec.Attempts = current.Attempts
	}
	rec.Attempts++
	if err != nil {
		rec.Error = err.Error()
	}
	rec.UpdatedAt = time.Now()
	j.records[path] = rec
	return rec.Attempts, j.appendLocked(journalEntry{Op: "put", Record: &rec})
}

// ClearFailure drops the recorded error for path so the next attempt starts
// from a clean record (the attempt budget is kept).
func (j *Journal) ClearFailure(path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec, ok := j.records[path]
	if !ok || rec.Error == "" {
		return nil
	}
	rec.Error = ""
	rec.UpdatedAt = time.Now()
	j.records[path] = rec
	return j.appendLocked(journalEntry{Op: "put", Record: &rec})
}

// Done removes path's record: the remote delete committed, or the delete was
// cancelled or restored locally.
func (j *Journal) Done(path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.records[path]; !ok {
		return nil
	}
	delete(j.records, path)
	return j.appendLocked(journalEntry{Op: "done", Path: path})
}

// DoneUnder removes every record strictly below dir: a directory delete (or a
// directory takeover) supersedes its children's queued deletes, while dir's own
// record - if any - is kept.
func (j *Journal) DoneUnder(dir string) error {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return nil
	}
	prefix := dir + "/"
	j.mu.Lock()
	defer j.mu.Unlock()
	dropped := make([]string, 0, len(j.records))
	for path := range j.records {
		if strings.HasPrefix(path, prefix) {
			dropped = append(dropped, path)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	for _, path := range dropped {
		delete(j.records, path)
	}
	return j.appendLocked(journalEntry{Op: "done_under", Path: dir})
}

// PersistenceError reports the first journal write failure. A journal without
// durability must be visible to callers instead of silently dropping intents.
func (j *Journal) PersistenceError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.persistErr
}

// Close flushes and closes the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

// ---- internals ----

func (j *Journal) load() (int, error) {
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("delete: open journal %s: %w", j.path, err)
	}
	j.file = f
	entries := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// A torn tail is expected after a crash mid-append: skip it and
			// keep the records that did decode.
			logging.L.Warnf("[VFS] delete journal: skipping corrupt line entries=%d err=%v", entries, err)
			continue
		}
		entries++
		switch entry.Op {
		case "put":
			if entry.Record != nil && entry.Record.Path != "" {
				j.records[entry.Record.Path] = *entry.Record
			}
		case "done":
			delete(j.records, entry.Path)
		case "done_under":
			prefix := strings.TrimSuffix(entry.Path, "/") + "/"
			for path := range j.records {
				if strings.HasPrefix(path, prefix) {
					delete(j.records, path)
				}
			}
		default:
			logging.L.Warnf("[VFS] delete journal: unknown op=%q", entry.Op)
		}
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("delete: read journal %s: %w", j.path, err)
	}
	j.entries = entries
	if j.shouldCompactLocked() {
		if err := j.compactLocked(); err != nil {
			return j.entries, err
		}
	}
	return j.entries, nil
}

// shouldCompactLocked reports whether the log has grown past the point where
// rewriting it (one line per live record) pays off: at least compactAt entries
// and more log lines than journalCompactFactor times the live record count.
func (j *Journal) shouldCompactLocked() bool {
	if j.entries < j.compactAt {
		return false
	}
	return j.entries > len(j.records)*journalCompactFactor
}

func (j *Journal) appendLocked(entry journalEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		j.recordPersistenceError(err)
		return err
	}
	line = append(line, '\n')
	if err := j.writeLocked(line); err != nil {
		return err
	}
	j.entries++

	// Rewrite the log once it carries more churn than its live records
	// justify, so a mount that deletes forever keeps a bounded journal.
	if j.shouldCompactLocked() {
		if err := j.compactLocked(); err != nil {
			j.recordPersistenceError(err)
			return err
		}
	}
	return nil
}

// writeLocked appends one line and flushes it to stable storage.
func (j *Journal) writeLocked(line []byte) error {
	if j.file == nil {
		f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			j.recordPersistenceError(err)
			return err
		}
		j.file = f
	}
	if _, err := j.file.Write(line); err != nil {
		j.recordPersistenceError(err)
		return err
	}
	if !j.durability {
		return nil
	}
	if err := j.file.Sync(); err != nil {
		j.recordPersistenceError(err)
		return err
	}
	return nil
}

// compactLocked rewrites the journal to one "put" line per live record through
// a temporary file, so a crash mid-compaction leaves the previous journal (and
// therefore every intent) intact.
func (j *Journal) compactLocked() error {
	if j.file == nil {
		return nil
	}
	tmp := j.path + ".compact"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("delete: compact journal %s: %w", tmp, err)
	}
	writer := bufio.NewWriter(f)
	for _, rec := range j.recordsLocked() {
		record := rec
		line, err := json.Marshal(journalEntry{Op: "put", Record: &record})
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if j.durability {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("delete: replace journal %s: %w", j.path, err)
	}
	if err := j.file.Close(); err != nil {
		return err
	}
	reopened, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		j.file = nil
		return fmt.Errorf("delete: reopen journal %s: %w", j.path, err)
	}
	j.file = reopened
	j.entries = len(j.records)
	if j.durability {
		if err := syncParentDir(j.path); err != nil {
			return err
		}
	}
	return nil
}

func (j *Journal) recordPersistenceError(err error) {
	if j.persistErr == nil {
		j.persistErr = err
	}
}

// syncParentDir fsyncs the directory holding path so a rename survives a crash.
func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
