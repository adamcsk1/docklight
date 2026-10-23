package management

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type blockedResponse struct {
	*httptest.ResponseRecorder
	started, release chan struct{}
}

func (w *blockedResponse) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestSlowJobResponseDoesNotBlockOutput(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	s.jobs["test"] = &storedJob{Job: Job{ID: "test", Status: "running"}}
	w := &blockedResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	r := httptest.NewRequest("GET", "/v1/jobs/test", nil)
	r.SetPathValue("id", "test")
	done := make(chan struct{})
	go func() { s.job(w, r); close(done) }()
	<-w.started
	defer func() { close(w.release); <-done }()
	written := make(chan struct{})
	go func() { _, _ = (jobWriter{s, "test"}).Write([]byte("progress")); close(written) }()
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("slow response holds job lock")
	}
}

func TestSlowSubmissionResponseDoesNotBlockJob(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	w := &blockedResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(`{"kind":"container","id":"`+testID+`","operation":"stop"}`))
	done := make(chan struct{})
	go func() { s.submit(w, r); close(done) }()
	<-w.started
	defer func() { close(w.release); <-done }()
	finished := make(chan struct{})
	go func() {
		for {
			s.mu.Lock()
			busy := s.busy
			s.mu.Unlock()
			if !busy {
				close(finished)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("slow submission response blocked execution")
	}
}

func TestSlowHelperInspectionDoesNotBlockOutput(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.e, testToken)
	if err != nil {
		t.Fatal(err)
	}
	s.jobs["update"] = &storedJob{Job: Job{ID: "update", Status: "updating"}, Helper: "helper"}
	s.jobs["other"] = &storedJob{Job: Job{ID: "other", Status: "success"}}
	started, release := make(chan struct{}), make(chan struct{})
	f.e.Run = func(ctx context.Context, dir string, out io.Writer, args ...string) error {
		close(started)
		<-release
		return context.DeadlineExceeded
	}
	done := make(chan struct{})
	go func() { s.refresh("update"); close(done) }()
	<-started
	defer func() { close(release); <-done }()
	written := make(chan struct{})
	go func() {
		_, _ = (jobWriter{s, "other"}).Write([]byte("progress"))
		if w := agentRequest(s, "GET", "/v1/jobs/other", "", testToken); w.Code != 200 {
			t.Error("unrelated result unavailable")
		}
		close(written)
	}()
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("helper inspection holds job lock")
	}
}
