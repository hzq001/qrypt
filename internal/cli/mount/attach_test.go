package mount

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yinzhenyu/qrypt/pkg/control"
)

type fakeAttachClient struct {
	mounts         []control.MountStatus
	listErr        error
	listFunc       func(context.Context) ([]control.MountStatus, error)
	listCalls      int
	closeIdleCalls int
	outcome        control.MountOutcome
	outcomeErr     error
	outcomeCalls   int
}

func (c *fakeAttachClient) ListMounts(ctx context.Context) ([]control.MountStatus, error) {
	c.listCalls++
	if c.listFunc != nil {
		return c.listFunc(ctx)
	}
	return c.mounts, c.listErr
}

func (c *fakeAttachClient) CloseIdleConnections() {
	c.closeIdleCalls++
}

func (c *fakeAttachClient) MountOutcome(context.Context, string) (control.MountOutcome, error) {
	c.outcomeCalls++
	return c.outcome, c.outcomeErr
}

func TestWatchAttachedMountOutcomes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		client         *fakeAttachClient
		wantErr        bool
		wantIsSession  bool
		wantDetach     bool
		wantConsumed   bool
		wantOutContain string
	}{
		{
			name: "detached exit 0",
			client: &fakeAttachClient{
				outcome: control.MountOutcome{ID: "m1", Active: false, Reason: "detached"},
			},
			wantConsumed: true,
		},
		{
			name: "external eject exit 0",
			client: &fakeAttachClient{
				outcome: control.MountOutcome{ID: "m1", Active: false, Reason: "external"},
			},
			wantConsumed:   true,
			wantOutContain: "Unmounted externally: /mnt/qrypt",
		},
		{
			name: "unexpected death exits non-zero",
			client: &fakeAttachClient{
				outcome: control.MountOutcome{ID: "m1", Active: false, Reason: "unexpected", Error: "short read on fuse device"},
			},
			wantErr:        true,
			wantIsSession:  true,
			wantConsumed:   true,
			wantOutContain: "short read on fuse device",
		},
		{
			name: "404 treated as detached exit 0",
			client: &fakeAttachClient{
				outcomeErr: &control.TransportError{Status: http.StatusNotFound, Code: control.CodeNotFound, Body: "not found"},
			},
			wantConsumed: true,
		},
		{
			name: "501 unsupported treated as detached exit 0",
			client: &fakeAttachClient{
				outcomeErr: &control.TransportError{Status: http.StatusNotImplemented, Code: control.CodeUnsupported, Body: "unsupported"},
			},
			wantConsumed: true,
		},
		{
			name: "list failure calls detach and returns error",
			client: &fakeAttachClient{
				listErr: errors.New("connection refused"),
			},
			wantErr:       true,
			wantIsSession: true,
			wantDetach:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			detached := false
			consumed := false
			err := watchAttachedMount(
				context.Background(),
				tc.client,
				"m1",
				"/mnt/qrypt",
				5*time.Millisecond,
				func() { detached = true },
				func() { consumed = true },
				&errBuf,
			)
			if tc.wantErr && err == nil {
				t.Fatalf("watchAttachedMount() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("watchAttachedMount() = %v, want nil", err)
			}
			if tc.wantIsSession && !errors.Is(err, errMountSessionEnded) {
				t.Fatalf("error = %v, want errMountSessionEnded", err)
			}
			if detached != tc.wantDetach {
				t.Fatalf("detached = %v, want %v", detached, tc.wantDetach)
			}
			if consumed != tc.wantConsumed {
				t.Fatalf("consumed = %v, want %v", consumed, tc.wantConsumed)
			}
			if tc.wantOutContain != "" && !strings.Contains(errBuf.String(), tc.wantOutContain) {
				t.Fatalf("stderr = %q, want %q", errBuf.String(), tc.wantOutContain)
			}
		})
	}
}

func TestWatchAttachedMountContextCancelDetaches(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	detached := false
	err := watchAttachedMount(
		ctx,
		&fakeAttachClient{},
		"m1",
		"/mnt/qrypt",
		50*time.Millisecond,
		func() { detached = true },
		func() {},
		&bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("watchAttachedMount() = %v, want nil", err)
	}
	if !detached {
		t.Fatal("detach was not called on context cancel")
	}
}

type attachHTTPMountService struct {
	mounts  []control.MountStatus
	outcome control.MountOutcome
}

func (s *attachHTTPMountService) ListMounts(context.Context) ([]control.MountStatus, error) {
	return s.mounts, nil
}
func (*attachHTTPMountService) AttachMount(context.Context, control.MountAttachRequest) (control.MountStatus, error) {
	return control.MountStatus{}, nil
}
func (*attachHTTPMountService) DetachMount(context.Context, string) error { return nil }
func (s *attachHTTPMountService) MountOutcome(context.Context, string) (control.MountOutcome, error) {
	return s.outcome, nil
}

func TestWatchAttachedMountWithControlClient(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"detached", "external", "unexpected"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			mountID := "/Users/me/Qrypt"
			svc := &attachHTTPMountService{
				outcome: control.MountOutcome{ID: mountID, Active: false, Reason: reason, Error: "session died"},
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/control/mounts":
					_, _ = w.Write([]byte(`{"mounts":[]}`))
				case strings.HasPrefix(r.URL.Path, "/v1/control/mounts/"):
					outcome, _ := svc.MountOutcome(r.Context(), mountID)
					switch outcome.Reason {
					case "external":
						_, _ = w.Write([]byte(`{"id":"/Users/me/Qrypt","active":false,"reason":"external"}`))
					case "unexpected":
						_, _ = w.Write([]byte(`{"id":"/Users/me/Qrypt","active":false,"reason":"unexpected","error":"session died"}`))
					default:
						_, _ = w.Write([]byte(`{"id":"/Users/me/Qrypt","active":false,"reason":"detached"}`))
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			client, err := control.NewClient(srv.URL)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			var errBuf bytes.Buffer
			consumed := false
			gotErr := watchAttachedMount(
				context.Background(),
				client,
				mountID,
				mountID,
				5*time.Millisecond,
				func() { t.Error("unexpected detach call") },
				func() { consumed = true },
				&errBuf,
			)
			if !consumed {
				t.Fatal("detach was not consumed")
			}
			if reason == "unexpected" {
				if !errors.Is(gotErr, errMountSessionEnded) {
					t.Fatalf("unexpected reason error = %v, want errMountSessionEnded", gotErr)
				}
			} else if gotErr != nil {
				t.Fatalf("%s reason error = %v, want nil", reason, gotErr)
			}
		})
	}
}

func TestWatchAttachedMountRetriesTransientPollErrors(t *testing.T) {
	t.Parallel()
	var calls int
	client := &fakeAttachClient{
		outcome: control.MountOutcome{ID: "m1", Active: false, Reason: "detached"},
	}
	client.listFunc = func(context.Context) ([]control.MountStatus, error) {
		calls++
		switch calls {
		case 1, 2:
			return nil, context.DeadlineExceeded
		case 3:
			return []control.MountStatus{{ID: "m1"}}, nil
		default:
			return nil, nil
		}
	}

	var errBuf bytes.Buffer
	detached := false
	consumed := false
	err := watchAttachedMount(
		context.Background(),
		client,
		"m1",
		"/mnt/qrypt",
		5*time.Millisecond,
		func() { detached = true },
		func() { consumed = true },
		&errBuf,
	)
	if err != nil {
		t.Fatalf("watchAttachedMount() = %v, want nil", err)
	}
	if detached {
		t.Fatal("detach was unexpectedly called on transient poll errors")
	}
	if !consumed {
		t.Fatal("detach was not consumed on clean detach outcome")
	}
	if client.listCalls != 4 {
		t.Fatalf("listCalls = %d, want 4", client.listCalls)
	}
	if client.closeIdleCalls != 2 {
		t.Fatalf("closeIdleCalls = %d, want 2", client.closeIdleCalls)
	}
}

func TestWatchAttachedMountResetsFailureCountAfterSuccess(t *testing.T) {
	t.Parallel()
	var calls int
	client := &fakeAttachClient{
		outcome: control.MountOutcome{ID: "m1", Active: false, Reason: "detached"},
	}
	client.listFunc = func(context.Context) ([]control.MountStatus, error) {
		calls++
		switch calls {
		case 1, 2, 4, 5:
			return nil, context.DeadlineExceeded
		case 3, 6:
			return []control.MountStatus{{ID: "m1"}}, nil
		default:
			return nil, nil
		}
	}

	var errBuf bytes.Buffer
	detached := false
	consumed := false
	err := watchAttachedMount(
		context.Background(),
		client,
		"m1",
		"/mnt/qrypt",
		5*time.Millisecond,
		func() { detached = true },
		func() { consumed = true },
		&errBuf,
	)
	if err != nil {
		t.Fatalf("watchAttachedMount() = %v, want nil", err)
	}
	if detached {
		t.Fatal("detach was unexpectedly called when poll failures were non-consecutive")
	}
	if !consumed {
		t.Fatal("detach was not consumed on clean detach outcome")
	}
	if client.closeIdleCalls != 4 {
		t.Fatalf("closeIdleCalls = %d, want 4", client.closeIdleCalls)
	}
}

func TestWatchAttachedMountDetachesCleanlyWhenCanceledDuringPollError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeAttachClient{
		listFunc: func(context.Context) ([]control.MountStatus, error) {
			cancel()
			return nil, context.DeadlineExceeded
		},
	}

	var errBuf bytes.Buffer
	detached := false
	err := watchAttachedMount(
		ctx,
		client,
		"m1",
		"/mnt/qrypt",
		5*time.Millisecond,
		func() { detached = true },
		func() {},
		&errBuf,
	)
	if err != nil {
		t.Fatalf("watchAttachedMount() = %v, want nil on context cancel", err)
	}
	if !detached {
		t.Fatal("detach was not called when canceled during poll error")
	}
}
