package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSONSuccess(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeJSON(w, map[string]string{"message": "ready"})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %v", w.Code, w.Header())
	}
	if got, want := w.Body.String(), "{\n  \"message\": \"ready\"\n}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestWriteJSONEncodingFailureReturnsInternalServerError(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	writeJSON(w, make(chan int))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("error content type = %q", w.Header().Get("Content-Type"))
	}
}

// A failed network write still commits the HTTP status before returning its
// error. Retrying with http.Error would send a second header and body.
type failedJSONResponseWriter struct {
	*httptest.ResponseRecorder
	statuses []int
	writes   int
}

func (w *failedJSONResponseWriter) WriteHeader(code int) {
	w.statuses = append(w.statuses, code)
	w.ResponseRecorder.WriteHeader(code)
}

func (w *failedJSONResponseWriter) Write(_ []byte) (int, error) {
	if len(w.statuses) == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestWriteJSONTransportFailureDoesNotWriteAgain(t *testing.T) {
	t.Parallel()

	w := &failedJSONResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	writeJSON(w, map[string]string{"message": "ready"})
	if len(w.statuses) != 1 || w.statuses[0] != http.StatusOK {
		t.Fatalf("statuses = %v, want a single 200", w.statuses)
	}
	if w.writes != 1 {
		t.Fatalf("writes = %d, want one attempt", w.writes)
	}
}
