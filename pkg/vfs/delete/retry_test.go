package delete

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/drive"
)

// TestRetryableFollowsDriveTaxonomy pins that the retry decision comes from the
// shared classification in pkg/drive, not from strings re-derived here.
func TestRetryableFollowsDriveTaxonomy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limit", drive.ErrRateLimit, true},
		{"deadline", context.DeadlineExceeded, true},
		{"network text", errors.New("network is unreachable"), true},
		{"server error text", errors.New("502 bad gateway"), true},
		{"not found", fs.ErrNotExist, false},
		{"permission", fs.ErrPermission, false},
		{"baidu errno 12", errors.New("baidu api errno 12: file not exist"), false},
		{"unclassified", errors.New("provider returned unusual response"), false},
		{"explicitly non retryable", drive.NonRetryable(errors.New("network is unreachable")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Retryable(tc.err); got != tc.want {
				t.Fatalf("Retryable(%v) = %t, want %t (category %q)", tc.err, got, tc.want, drive.ErrorCategory(tc.err))
			}
		})
	}
}

// TestRetryPlanBoundsAttempts checks the budget and backoff schedule a failed
// delete walks through, including the case where no attempt may follow.
func TestRetryPlanBoundsAttempts(t *testing.T) {
	t.Parallel()
	transient := errors.New("network is unreachable")
	want := []time.Duration{2 * time.Second, 10 * time.Second, time.Minute, 5 * time.Minute}
	for i, expected := range want {
		attempts := i + 1
		delay, retry := RetryPlan(attempts, transient, true)
		if !retry || delay != expected {
			t.Fatalf("RetryPlan(%d) = (%s, %t), want (%s, true)", attempts, delay, retry, expected)
		}
	}
	if _, retry := RetryPlan(MaxAttempts, transient, true); retry {
		t.Fatalf("attempt %d must be final: the budget is %d", MaxAttempts, MaxAttempts)
	}
	if _, retry := RetryPlan(MaxAttempts+1, transient, true); retry {
		t.Fatal("attempts beyond the budget must not retry")
	}
	if _, retry := RetryPlan(1, errors.New("baidu api errno 12: file not exist"), true); retry {
		t.Fatal("a deterministic failure must never be retried")
	}
	if _, retry := RetryPlan(1, transient, false); retry {
		t.Fatal("a cancelled lifecycle must not arm a retry")
	}
}

// TestResumePlanAfterRestart covers replay decisions: a delete that never ran
// restarts immediately, a failed one keeps the schedule its error implies, and
// a spent budget stays spent (the record is reported, not retried again).
func TestResumePlanAfterRestart(t *testing.T) {
	t.Parallel()
	queued := JournalRecord{Path: "/x.bin", Attempts: 0}
	if delay, retry := ResumePlan(queued, true); !retry || delay != 0 {
		t.Fatalf("queued record = (%s, %t), want an immediate attempt", delay, retry)
	}
	transient := JournalRecord{Path: "/x.bin", Attempts: 2, Error: "network is unreachable"}
	if delay, retry := ResumePlan(transient, true); !retry || delay != 10*time.Second {
		t.Fatalf("transient record = (%s, %t), want (10s, true)", delay, retry)
	}
	deterministic := JournalRecord{Path: "/x.bin", Attempts: 1, Error: "baidu api errno 12: file not exist"}
	if _, retry := ResumePlan(deterministic, true); retry {
		t.Fatal("a deterministic failure must stay reported instead of retried")
	}
	spent := JournalRecord{Path: "/x.bin", Attempts: MaxAttempts, Error: "network is unreachable"}
	if _, retry := ResumePlan(spent, true); retry {
		t.Fatal("a spent budget must not restart after a restart")
	}
	if _, retry := ResumePlan(queued, false); retry {
		t.Fatal("a cancelled lifecycle must not replay work")
	}
}
