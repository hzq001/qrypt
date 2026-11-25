package mount

import (
	"errors"
	"testing"
)

func TestSessionDoneStartsOpenAndClosesOnFinish(t *testing.T) {
	t.Parallel()
	session := &Session{done: make(chan struct{})}
	select {
	case <-session.Done():
		t.Fatal("Done closed before the session ended")
	default:
	}

	cause := errors.New("short read on fuse device")
	session.finish(cause)

	select {
	case <-session.Done():
	default:
		t.Fatal("Done still open after finish")
	}
	if !errors.Is(session.Err(), cause) {
		t.Fatalf("Err = %v, want %v", session.Err(), cause)
	}
}

func TestSessionFinishKeepsFirstCause(t *testing.T) {
	t.Parallel()
	session := &Session{done: make(chan struct{})}
	first := errors.New("first")
	second := errors.New("second")

	session.finish(first)
	session.finish(second)

	if !errors.Is(session.Err(), first) {
		t.Fatalf("Err = %v, want the first cause %v", session.Err(), first)
	}
}

func TestSessionDoneNilWithoutChannel(t *testing.T) {
	t.Parallel()
	// Sessions built by hand (older tests and embedders) carry no channel.
	// Done must stay nil so a select on it simply never fires.
	var session Session
	if session.Done() != nil {
		t.Fatal("Done should be nil for a session without a done channel")
	}
	if session.Err() != nil {
		t.Fatalf("Err = %v, want nil", session.Err())
	}
	session.finish(errors.New("ignored"))
	if session.Err() != nil {
		t.Fatalf("Err = %v after finish on a channel-less session, want nil", session.Err())
	}
}

func TestNilSessionDoneAndErr(t *testing.T) {
	t.Parallel()
	var session *Session
	if session.Done() != nil {
		t.Fatal("Done should be nil for a nil session")
	}
	if session.Err() != nil {
		t.Fatalf("Err = %v, want nil", session.Err())
	}
	session.finish(errors.New("ignored"))
}
