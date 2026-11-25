package delete

import (
	"errors"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

// Remote-delete retry budget. A delete that failed for a transient reason
// (network, timeout, rate limit, 5xx) is re-armed with a growing backoff; a
// deterministic failure (not found, conflict, permission, ...) is never
// retried, because the driver already told us the answer will not change.
const (
	// MaxAttempts caps the remote attempts spent on one queued delete,
	// across retries and process restarts (the journal keeps the count).
	MaxAttempts = 5
)

// retryDelays is the backoff after attempt 1, 2, 3 and 4.
var retryDelays = [...]time.Duration{
	2 * time.Second,
	10 * time.Second,
	time.Minute,
	5 * time.Minute,
}

// Retryable reports whether a failed remote delete is worth another attempt.
// The classification comes from pkg/drive's shared taxonomy - never from the
// error text here.
func Retryable(err error) bool {
	if err == nil || drive.IsNonRetryable(err) {
		return false
	}
	return drive.RetryableCategory(drive.ErrorCategory(err))
}

// RetryPlan decides what happens after a failed attempt. attempts is the total
// number of remote attempts already made (including the one that just failed).
// canRetry is false when no retry target is installed or the context is gone.
func RetryPlan(attempts int, err error, canRetry bool) (time.Duration, bool) {
	if !canRetry || attempts < 0 || attempts >= MaxAttempts || !Retryable(err) {
		return 0, false
	}
	index := attempts - 1
	if index < 0 {
		index = 0
	}
	if index >= len(retryDelays) {
		return 0, false
	}
	return retryDelays[index], true
}

// ResumePlan decides what a journal record does after a restart. A record whose
// delete never ran is re-armed immediately; a failed one keeps the decision its
// own error text implies (the classification is text-based because the original
// error value did not survive the restart). A spent budget stays spent: the
// record is reported, not retried again.
func ResumePlan(rec JournalRecord, canRetry bool) (time.Duration, bool) {
	if !canRetry || rec.Attempts < 0 || rec.Attempts >= MaxAttempts {
		return 0, false
	}
	if rec.Attempts == 0 || rec.Error == "" {
		return 0, true
	}
	return RetryPlan(rec.Attempts, errors.New(rec.Error), true)
}
