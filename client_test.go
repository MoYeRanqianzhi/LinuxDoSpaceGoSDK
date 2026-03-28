package linuxdospace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testOwnerUsername = "testuser"
	testNamespace     = testOwnerUsername + "-mail.linuxdo.space"
	testLegacyAlias   = testOwnerUsername + ".linuxdo.space"
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
	if !server.waitForSubscribers(1, 2*time.Second) {
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

	server.publish("alice@"+testNamespace, rawRFC822("alice@"+testNamespace, "hello"))

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
	if !server.waitForSubscribers(1, 2*time.Second) {
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

	server.publish("alice@"+testNamespace, rawRFC822("alice@"+testNamespace, "before"))
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

	server.publish("alice@"+testNamespace, rawRFC822("alice@"+testNamespace, "after"))

	select {
	case item := <-ch:
		if item.Subject != "after" {
			t.Fatalf("expected only post-listen message, got %q", item.Subject)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive post-listen message")
	}
}

func TestClientReconnectsAfterGracefulEOFAndExposesFatalErr(t *testing.T) {
	transport := &sequentialRoundTripper{
		responses: []roundTripResponse{
			streamRoundTripResponse(http.StatusOK, `{"type":"ready","token_public_id":"tok123","owner_username":"`+testOwnerUsername+`"}`+"\n"),
			streamRoundTripResponse(http.StatusOK, `{"type":"ready","token_public_id":"tok123","owner_username":"`+testOwnerUsername+`"}`+"\n"),
			streamRoundTripResponse(http.StatusUnauthorized, "token rejected"),
		},
	}
	httpClient := &http.Client{Transport: transport}

	client, err := NewClient(
		"token",
		WithBaseURL("http://localhost:8787"),
		WithHTTPClient(httpClient),
		WithReconnectDelay(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if authErr, ok := client.Err().(*AuthenticationError); ok {
			if authErr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unexpected auth status code: %d", authErr.StatusCode)
			}
			if transport.calls.Load() < 2 {
				t.Fatalf("expected reconnect attempt before fatal auth, calls=%d", transport.calls.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected fatal auth error after reconnect, calls=%d, err=%v", transport.calls.Load(), client.Err())
}

func TestDroppedCountersExposeBackpressureLoss(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(1, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	allCtx, allCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer allCancel()
	allCh, stopAll := client.Listen(allCtx)
	defer stopAll()

	mailbox, err := client.BindExact("alice", SuffixLinuxdoSpace, false)
	if err != nil {
		t.Fatalf("bind exact: %v", err)
	}
	defer mailbox.Close()

	boxCtx, boxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer boxCancel()
	boxCh, err := mailbox.Listen(boxCtx)
	if err != nil {
		t.Fatalf("mailbox listen: %v", err)
	}

	// Fill both queues beyond their fixed buffer size without consuming from them yet.
	for i := 0; i < listenerBufferSize*32; i++ {
		server.publish("alice@"+testNamespace, rawRFC822("alice@"+testNamespace, "load"))
	}
	time.Sleep(500 * time.Millisecond)

	if client.Dropped() == 0 {
		t.Fatal("expected full-stream dropped counter to increase")
	}
	if mailbox.Dropped() == 0 {
		t.Fatal("expected mailbox dropped counter to increase")
	}

	// Drain and release resources so the close path remains clean.
drainAll:
	for {
		select {
		case <-allCh:
		default:
			break drainAll
		}
	}
drainBox:
	for {
		select {
		case <-boxCh:
		default:
			break drainBox
		}
	}
}

func TestFullStreamProjectsOneMessageForMultiRecipientEvent(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(1, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	allCh, stop := client.Listen(ctx)
	defer stop()

	server.publishMany([]string{"alice@" + testNamespace, "bob@" + testNamespace}, rawRFC822("alice@"+testNamespace, "multi"))

	select {
	case got := <-allCh:
		if got.Subject != "multi" {
			t.Fatalf("expected multi-recipient subject, got %q", got.Subject)
		}
		if len(got.Recipients) != 2 {
			t.Fatalf("expected one full-stream projection carrying both recipients, got %+v", got.Recipients)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive full-stream projection")
	}

	select {
	case unexpected := <-allCh:
		t.Fatalf("expected one full-stream projection for one upstream mail event, got extra %+v", unexpected)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSemanticSuffixAlsoMatchesLegacyOwnerNamespace(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(1, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	mailbox, err := client.BindExact("alice", SuffixLinuxdoSpace, false)
	if err != nil {
		t.Fatalf("bind semantic suffix: %v", err)
	}
	defer mailbox.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := mailbox.Listen(ctx)
	if err != nil {
		t.Fatalf("listen semantic suffix: %v", err)
	}

	server.publish("alice@"+testLegacyAlias, rawRFC822("alice@"+testLegacyAlias, "legacy namespace"))

	select {
	case got := <-ch:
		if got.Address != "alice@"+testLegacyAlias {
			t.Fatalf("expected semantic suffix to accept legacy namespace address, got %q", got.Address)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("semantic suffix binding did not receive legacy namespace message")
	}
}

func TestSemanticSuffixDefaultsToOwnerMailNamespaceAndSyncsFilters(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(1, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	mailbox, err := client.BindExact("alice", SuffixLinuxdoSpace, false)
	if err != nil {
		t.Fatalf("bind semantic suffix: %v", err)
	}
	if mailbox.Address() != "alice@"+testNamespace {
		t.Fatalf("expected owner-mail default namespace, got %q", mailbox.Address())
	}
	if !server.waitForFilterSuffixes([]string{""}, 2*time.Second) {
		t.Fatalf("expected synced suffix fragments [''], got %+v", server.currentFilterSuffixes())
	}

	mailbox.Close()
	if !server.waitForFilterSuffixes([]string{}, 2*time.Second) {
		t.Fatalf("expected suffix fragments to clear after mailbox close, got %+v", server.currentFilterSuffixes())
	}
}

func TestSemanticSuffixWithDynamicFragmentSyncsFilters(t *testing.T) {
	server := newFakeStreamServer(t)
	defer server.close()

	client, err := NewClient("token", WithBaseURL(server.baseURL()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = client.Close() }()
	if !server.waitForSubscribers(1, 2*time.Second) {
		t.Fatal("stream subscriber was not ready")
	}

	mailbox, err := client.BindExact("alice", SuffixLinuxdoSpace.WithSuffix("Foo --- Bar"), false)
	if err != nil {
		t.Fatalf("bind semantic suffix with fragment: %v", err)
	}
	defer mailbox.Close()
	if mailbox.Address() != "alice@testuser-mailfoo-bar.linuxdo.space" {
		t.Fatalf("expected owner-mail dynamic namespace, got %q", mailbox.Address())
	}
	if !server.waitForFilterSuffixes([]string{"foo-bar"}, 2*time.Second) {
		t.Fatalf("expected synced suffix fragments ['foo-bar'], got %+v", server.currentFilterSuffixes())
	}
}

type fakeStreamServer struct {
	server      *httptest.Server
	subscribers chan chan []byte
	publishCh   chan []byte
	stopCh      chan struct{}
	mu          sync.Mutex
	subCount    int
	filterSet   []string
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
			"owner_username":  testOwnerUsername,
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
	mux.HandleFunc(streamFiltersPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		payload := struct {
			Suffixes []string `json:"suffixes"`
		}{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		normalized := make([]string, 0, len(payload.Suffixes))
		seen := map[string]struct{}{}
		for _, suffix := range payload.Suffixes {
			current := strings.ToLower(strings.TrimSpace(suffix))
			if _, ok := seen[current]; ok {
				continue
			}
			seen[current] = struct{}{}
			normalized = append(normalized, current)
		}
		slices.Sort(normalized)

		f.mu.Lock()
		f.filterSet = append([]string(nil), normalized...)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"suffixes": normalized,
		})
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

func (f *fakeStreamServer) waitForFilterSuffixes(expected []string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	normalizedExpected := append([]string(nil), expected...)
	slices.Sort(normalizedExpected)
	for time.Now().Before(deadline) {
		if slices.Equal(f.currentFilterSuffixes(), normalizedExpected) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (f *fakeStreamServer) currentFilterSuffixes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.filterSet...)
}

func (f *fakeStreamServer) publish(recipient string, raw []byte) {
	f.publishMany([]string{recipient}, raw)
}

func (f *fakeStreamServer) publishMany(recipients []string, raw []byte) {
	f.publishCh <- encodeEvent(map[string]any{
		"type":                   "mail",
		"original_envelope_from": "bounce@example.com",
		"original_recipients":    recipients,
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

type roundTripResponse struct {
	statusCode int
	body       string
}

type sequentialRoundTripper struct {
	calls     atomic.Int32
	mu        sync.Mutex
	responses []roundTripResponse
}

func (s *sequentialRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.responses) == 0 {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return &http.Response{
		StatusCode: response.statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response.body)),
	}, nil
}

func streamRoundTripResponse(statusCode int, body string) roundTripResponse {
	return roundTripResponse{
		statusCode: statusCode,
		body:       body,
	}
}
