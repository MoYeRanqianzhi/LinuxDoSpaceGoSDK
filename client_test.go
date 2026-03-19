package linuxdospace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRejectsNonLocalHTTPBaseURL(t *testing.T) {
	_, err := NewClient("token", WithBaseURL("http://example.com"))
	if err == nil {
		t.Fatal("expected non-local http base URL to fail")
	}
}

func TestOrderedMatchingAndAllowOverlap(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(2, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	catchAll, err := client.BindPattern(".*", SuffixLinuxdoSpace, true)
	if err != nil {
		t.Fatalf("bind catch-all: %v", err)
	}
	defer catchAll.Close()

	alice, err := client.BindExact("alice", SuffixLinuxdoSpace, false)
	if err != nil {
		t.Fatalf("bind alice: %v", err)
	}
	defer alice.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	catchCh, err := catchAll.Listen(ctx)
	if err != nil {
		t.Fatalf("catch-all listen: %v", err)
	}
	aliceCh, err := alice.Listen(ctx)
	if err != nil {
		t.Fatalf("alice listen: %v", err)
	}

	server.publish("alice@linuxdo.space", rawRFC822("alice@linuxdo.space", "hello"))

	select {
	case <-catchCh:
	case <-time.After(2 * time.Second):
		t.Fatal("catch-all did not receive message")
	}
	select {
	case <-aliceCh:
	case <-time.After(2 * time.Second):
		t.Fatal("exact binding did not receive message")
	}
}

func TestMailboxNoBackfillBeforeListen(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(2, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	alice, err := client.BindExact("alice", SuffixLinuxdoSpace, false)
	if err != nil {
		t.Fatalf("bind alice: %v", err)
	}
	defer alice.Close()

	// Use a full-stream listener to make sure the "before" message is already
	// consumed by the SDK before mailbox listen starts. This avoids race-based
	// false positives where the message had not reached the SDK yet.
	allCtx, allCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer allCancel()
	allCh, stopAll := client.Listen(allCtx)
	defer stopAll()

	server.publish("alice@linuxdo.space", rawRFC822("alice@linuxdo.space", "before"))
	select {
	case got := <-allCh:
		if got.Subject != "before" {
			t.Fatalf("expected pre-listen full stream message, got %q", got.Subject)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive pre-listen full stream message")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := alice.Listen(ctx)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server.publish("alice@linuxdo.space", rawRFC822("alice@linuxdo.space", "after"))

	select {
	case item := <-ch:
		if item.Subject != "after" {
			t.Fatalf("expected only post-listen message, got %q", item.Subject)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive post-listen message")
	}
}

type fakeStreamServer struct {
	server      *httptest.Server
	subscribers chan chan []byte
	publishCh   chan []byte
	stopCh      chan struct{}
	mu          sync.Mutex
	subCount    int
}

func newFakeStreamServer(t *testing.T) *fakeStreamServer {
	t.Helper()

	f := &fakeStreamServer{
		subscribers: make(chan chan []byte, 8),
		publishCh:   make(chan []byte, 32),
		stopCh:      make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeEvent(map[string]any{
			"type":            "ready",
			"token_public_id": "tok123",
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		sub := make(chan []byte, 16)
		f.subscribers <- sub
		f.mu.Lock()
		f.subCount++
		f.mu.Unlock()

		for {
			select {
			case <-r.Context().Done():
				return
			case payload := <-sub:
				_, _ = w.Write(payload)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	})

	f.server = httptest.NewServer(mux)
	go func() {
		subs := []chan []byte{}
		for {
			select {
			case <-f.stopCh:
				return
			case sub := <-f.subscribers:
				subs = append(subs, sub)
			case payload := <-f.publishCh:
				for _, sub := range subs {
					select {
					case sub <- payload:
					default:
					}
				}
			}
		}
	}()

	return f
}

func (f *fakeStreamServer) waitForSubscribers(expected int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		count := f.subCount
		f.mu.Unlock()
		if count >= expected {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (f *fakeStreamServer) baseURL() string {
	return f.server.URL
}

func (f *fakeStreamServer) publish(recipient string, raw []byte) {
	f.publishCh <- encodeEvent(map[string]any{
		"type":                   "mail",
		"original_envelope_from": "bounce@example.com",
		"original_recipients":    []string{recipient},
		"received_at":            "2026-03-20T10:11:12Z",
		"raw_message_base64":     base64.StdEncoding.EncodeToString(raw),
	})
}

func (f *fakeStreamServer) close() {
	close(f.stopCh)
	f.server.Close()
}

func encodeEvent(payload map[string]any) []byte {
	out, _ := json.Marshal(payload)
	return append(out, '\n')
}

func rawRFC822(recipient string, subject string) []byte {
	return []byte("From: sender@example.com\r\nTo: " + recipient + "\r\nSubject: " + subject + "\r\n\r\nHello")
}
