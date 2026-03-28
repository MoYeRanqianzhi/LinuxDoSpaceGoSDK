package linuxdospace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultBaseURL is the production API origin.
	DefaultBaseURL = "https://api.linuxdo.space"
	// streamPath is the protocol-defined stream endpoint path.
	streamPath = "/v1/token/email/stream"
	// streamFiltersPath is the protocol-defined dynamic mailbox-filter endpoint.
	streamFiltersPath = "/v1/token/email/filters"

	defaultConnectTimeout = 10 * time.Second
	defaultSocketTimeout  = 30 * time.Second
	defaultReconnectDelay = 300 * time.Millisecond

	listenerBufferSize = 128
	scannerMaxToken    = 4 * 1024 * 1024
)

// Option mutates Client construction options.
type Option func(*clientOptions) error

type clientOptions struct {
	baseURL        string
	connectTimeout time.Duration
	socketTimeout  time.Duration
	reconnectDelay time.Duration
	httpClient     *http.Client
}

// WithBaseURL sets API origin.
func WithBaseURL(baseURL string) Option {
	return func(o *clientOptions) error {
		o.baseURL = strings.TrimSpace(baseURL)
		return nil
	}
}

// WithConnectTimeout sets initial connect timeout.
func WithConnectTimeout(timeout time.Duration) Option {
	return func(o *clientOptions) error {
		o.connectTimeout = timeout
		return nil
	}
}

// WithSocketTimeout sets the stream idle timeout budget used by callers and
// custom clients. It is not applied as a total request lifetime timeout to the
// built-in long-lived stream client.
func WithSocketTimeout(timeout time.Duration) Option {
	return func(o *clientOptions) error {
		o.socketTimeout = timeout
		return nil
	}
}

// WithReconnectDelay sets reconnect delay for recoverable failures.
func WithReconnectDelay(delay time.Duration) Option {
	return func(o *clientOptions) error {
		o.reconnectDelay = delay
		return nil
	}
}

// WithHTTPClient injects custom HTTP client.
func WithHTTPClient(client *http.Client) Option {
	return func(o *clientOptions) error {
		o.httpClient = client
		return nil
	}
}

// Client owns one upstream stream and local routing state.
type Client struct {
	token string

	baseURL        string
	connectTimeout time.Duration
	httpClient     *http.Client
	socketTimeout  time.Duration
	reconnectDelay time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu             sync.RWMutex
	initialReadyCh chan struct{}
	initialReadyOK bool
	initialErr     error
	closed         bool
	fatalErr       error
	allListeners   map[int]chan MailMessage
	nextListenerID int
	bindings       map[string][]*mailBinding
	ownerUsername  string
	activeResp     io.Closer
	filtersSynced  bool
	filterSuffixes []string
	dropped        atomic.Uint64
}

type mailBinding struct {
	mode         string
	suffix       string
	allowOverlap bool
	prefix       string
	pattern      *regexp.Regexp
	mailbox      *Mailbox
}

func (b *mailBinding) matches(localPart string) bool {
	if b.mode == "exact" {
		return b.prefix == localPart
	}
	if b.mode == "pattern" && b.pattern != nil {
		return b.pattern.MatchString(localPart)
	}
	return false
}

// Mailbox is one local binding target.
//
// Queue activation starts only when Listen() starts.
type Mailbox struct {
	client       *Client
	mode         string
	suffix       string
	allowOverlap bool
	prefix       string
	pattern      string

	mu       sync.Mutex
	closed   bool
	active   bool
	listener chan MailMessage
	dropped  atomic.Uint64
}

// Mode reports binding mode: exact or pattern.
func (m *Mailbox) Mode() string { return m.mode }

// Suffix reports binding suffix.
func (m *Mailbox) Suffix() string { return m.suffix }

// AllowOverlap reports route overlap behavior.
func (m *Mailbox) AllowOverlap() bool { return m.allowOverlap }

// Address reports concrete mailbox address for exact mode.
func (m *Mailbox) Address() string {
	if m.mode != "exact" || m.prefix == "" || m.suffix == "" {
		return ""
	}
	return m.prefix + "@" + m.suffix
}

// Pattern reports pattern text for regex mode.
func (m *Mailbox) Pattern() string { return m.pattern }

// Closed reports whether mailbox is already unbound.
func (m *Mailbox) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Err exposes the terminal client error that closed this mailbox, if any.
func (m *Mailbox) Err() error {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Err()
}

// Dropped reports how many messages were dropped because the mailbox listener queue was full.
func (m *Mailbox) Dropped() uint64 {
	if m == nil {
		return 0
	}
	return m.dropped.Load()
}

// Listen starts mailbox-level consumption and returns a message channel.
//
// One mailbox only supports one active listener at a time.
func (m *Mailbox) Listen(ctx context.Context) (<-chan MailMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("mailbox is closed")
	}
	if m.active {
		return nil, errors.New("mailbox already has an active listener")
	}

	ch := make(chan MailMessage, listenerBufferSize)
	m.listener = ch
	m.active = true
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.listener == ch {
			close(ch)
			m.listener = nil
			m.active = false
		}
	}()
	return ch, nil
}

func (m *Mailbox) enqueue(msg MailMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.active || m.listener == nil {
		return
	}
	select {
	case m.listener <- msg:
	default:
		m.dropped.Add(1)
	}
}

func (m *Mailbox) forceCloseByClient() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.listener != nil {
		close(m.listener)
		m.listener = nil
	}
	m.active = false
}

// Close unbinds mailbox and closes active listener channel.
func (m *Mailbox) Close() {
	m.mu.Lock()
	alreadyClosed := m.closed
	m.closed = true
	if m.listener != nil {
		close(m.listener)
		m.listener = nil
	}
	m.active = false
	m.mu.Unlock()
	if alreadyClosed {
		return
	}
	m.client.unregisterMailbox(m)
}

type streamEvent struct {
	Type string `json:"type"`

	OwnerUsername        string   `json:"owner_username"`
	OriginalEnvelopeFrom string   `json:"original_envelope_from"`
	OriginalRecipients   []string `json:"original_recipients"`
	ReceivedAt           string   `json:"received_at"`
	RawMessageBase64     string   `json:"raw_message_base64"`
}

type streamReadResult struct {
	line []byte
	err  error
	eof  bool
}

// NewClient creates one client and performs initial connection attempt.
func NewClient(token string, options ...Option) (*Client, error) {
	normalizedToken := strings.TrimSpace(token)
	if normalizedToken == "" {
		return nil, errors.New("token must not be empty")
	}

	cfg := clientOptions{
		baseURL:        DefaultBaseURL,
		connectTimeout: defaultConnectTimeout,
		socketTimeout:  defaultSocketTimeout,
		reconnectDelay: defaultReconnectDelay,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.connectTimeout <= 0 {
		return nil, errors.New("connect timeout must be > 0")
	}
	if cfg.socketTimeout <= 0 {
		return nil, errors.New("socket timeout must be > 0")
	}
	if cfg.reconnectDelay <= 0 {
		return nil, errors.New("reconnect delay must be > 0")
	}

	baseURL, err := normalizeBaseURL(cfg.baseURL)
	if err != nil {
		return nil, err
	}

	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: cfg.connectTimeout}).DialContext,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   cfg.connectTimeout,
				ResponseHeaderTimeout: cfg.connectTimeout,
			},
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		token:          normalizedToken,
		baseURL:        baseURL,
		connectTimeout: cfg.connectTimeout,
		httpClient:     httpClient,
		socketTimeout:  cfg.socketTimeout,
		reconnectDelay: cfg.reconnectDelay,
		ctx:            ctx,
		cancel:         cancel,
		initialReadyCh: make(chan struct{}),
		allListeners:   map[int]chan MailMessage{},
		bindings:       map[string][]*mailBinding{},
	}

	client.wg.Add(1)
	go client.streamLoop()
	select {
	case <-client.initialReadyCh:
		client.mu.RLock()
		initialOK := client.initialReadyOK
		initialErr := client.initialErr
		client.mu.RUnlock()
		if !initialOK {
			cancel()
			client.wg.Wait()
			if initialErr != nil {
				return nil, initialErr
			}
			return nil, &StreamError{Message: "mail stream ended before ready event"}
		}
	case <-time.After(cfg.connectTimeout):
		cancel()
		client.wg.Wait()
		return nil, &StreamError{Message: "timed out while opening LinuxDoSpace stream"}
	}
	return client, nil
}

// Listen registers full-stream listener for this token.
func (c *Client) Listen(ctx context.Context) (<-chan MailMessage, func()) {
	ch := make(chan MailMessage, listenerBufferSize)
	c.mu.Lock()
	if c.closed || c.fatalErr != nil {
		c.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	id := c.nextListenerID
	c.nextListenerID++
	c.allListeners[id] = ch
	c.mu.Unlock()

	stop := func() {
		c.mu.Lock()
		listener, ok := c.allListeners[id]
		if ok {
			delete(c.allListeners, id)
		}
		c.mu.Unlock()
		if ok {
			close(listener)
		}
	}

	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-c.ctx.Done():
			stop()
		}
	}()
	return ch, stop
}

// Err exposes the terminal fatal stream error, if the client stopped because of one.
func (c *Client) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fatalErr
}

// Dropped reports how many full-stream messages were dropped because listener queues were full.
func (c *Client) Dropped() uint64 {
	return c.dropped.Load()
}

// BindExact binds one exact prefix+suffix mailbox rule.
//
// `suffix` may be a `Suffix`, a `SemanticSuffix`, or one literal suffix string.
func (c *Client) BindExact(prefix string, suffix any, allowOverlap bool) (*Mailbox, error) {
	if err := c.ensureUsable(); err != nil {
		return nil, err
	}
	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	normalizedSuffix, err := c.resolveBindingSuffix(suffix)
	if normalizedPrefix == "" {
		return nil, errors.New("prefix must not be empty")
	}
	if err != nil {
		return nil, err
	}

	mailbox := &Mailbox{
		client:       c,
		mode:         "exact",
		suffix:       normalizedSuffix,
		allowOverlap: allowOverlap,
		prefix:       normalizedPrefix,
	}
	binding := &mailBinding{
		mode:         "exact",
		suffix:       normalizedSuffix,
		allowOverlap: allowOverlap,
		prefix:       normalizedPrefix,
		mailbox:      mailbox,
	}
	if err := c.registerBinding(binding); err != nil {
		return nil, err
	}
	return mailbox, nil
}

// BindPattern binds one regex+suffix mailbox rule.
//
// `suffix` may be a `Suffix`, a `SemanticSuffix`, or one literal suffix string.
func (c *Client) BindPattern(pattern string, suffix any, allowOverlap bool) (*Mailbox, error) {
	if err := c.ensureUsable(); err != nil {
		return nil, err
	}
	normalizedPattern := strings.TrimSpace(pattern)
	normalizedSuffix, err := c.resolveBindingSuffix(suffix)
	if normalizedPattern == "" {
		return nil, errors.New("pattern must not be empty")
	}
	if err != nil {
		return nil, err
	}
	compiled, err := regexp.Compile("^" + normalizedPattern + "$")
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	mailbox := &Mailbox{
		client:       c,
		mode:         "pattern",
		suffix:       normalizedSuffix,
		allowOverlap: allowOverlap,
		pattern:      normalizedPattern,
	}
	binding := &mailBinding{
		mode:         "pattern",
		suffix:       normalizedSuffix,
		allowOverlap: allowOverlap,
		pattern:      compiled,
		mailbox:      mailbox,
	}
	if err := c.registerBinding(binding); err != nil {
		return nil, err
	}
	return mailbox, nil
}

// Route resolves current local mailbox matches for message.Address.
func (c *Client) Route(message MailMessage) []*Mailbox {
	if err := c.ensureUsable(); err != nil {
		return nil
	}
	bindings := c.matchBindings(message.Address)
	if len(bindings) == 0 {
		return nil
	}
	out := make([]*Mailbox, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, binding.mailbox)
	}
	return out
}

// Close shuts down stream loop and local listeners.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.cancel()
	if c.activeResp != nil {
		_ = c.activeResp.Close()
		c.activeResp = nil
	}

	fullListeners := make([]chan MailMessage, 0, len(c.allListeners))
	for _, ch := range c.allListeners {
		fullListeners = append(fullListeners, ch)
	}
	c.allListeners = map[int]chan MailMessage{}

	unique := map[*Mailbox]struct{}{}
	for _, chain := range c.bindings {
		for _, binding := range chain {
			unique[binding.mailbox] = struct{}{}
		}
	}
	c.bindings = map[string][]*mailBinding{}
	c.mu.Unlock()

	for _, ch := range fullListeners {
		close(ch)
	}
	for mailbox := range unique {
		mailbox.forceCloseByClient()
	}
	c.wg.Wait()
	return nil
}

func (c *Client) streamLoop() {
	defer c.wg.Done()
	initialAttempt := true
	for {
		if c.isClosed() {
			return
		}
		err := c.consumeOnce(c.ctx)
		if c.isClosed() {
			return
		}
		if err == nil {
			if initialAttempt {
				c.signalInitialReady(false, &StreamError{Message: "mail stream ended before ready event"})
			}
			err = &StreamError{Message: "mail stream ended and will reconnect"}
		}
		var authErr *AuthenticationError
		if errors.As(err, &authErr) {
			if initialAttempt {
				c.signalInitialReady(false, err)
			}
			c.failFatal(err)
			return
		}
		if initialAttempt {
			c.signalInitialReady(false, err)
		}
		initialAttempt = false

		timer := time.NewTimer(c.reconnectDelay)
		select {
		case <-timer.C:
		case <-c.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (c *Client) failFatal(err error) {
	c.mu.Lock()
	if c.fatalErr != nil {
		c.mu.Unlock()
		return
	}
	c.fatalErr = err
	c.closed = true
	c.cancel()
	if c.activeResp != nil {
		_ = c.activeResp.Close()
		c.activeResp = nil
	}

	fullListeners := make([]chan MailMessage, 0, len(c.allListeners))
	for _, ch := range c.allListeners {
		fullListeners = append(fullListeners, ch)
	}
	c.allListeners = map[int]chan MailMessage{}

	unique := map[*Mailbox]struct{}{}
	for _, chain := range c.bindings {
		for _, binding := range chain {
			unique[binding.mailbox] = struct{}{}
		}
	}
	c.bindings = map[string][]*mailBinding{}
	c.mu.Unlock()

	for _, ch := range fullListeners {
		close(ch)
	}
	for mailbox := range unique {
		mailbox.forceCloseByClient()
	}
}

func (c *Client) consumeOnce(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+streamPath, nil)
	if err != nil {
		return &StreamError{Message: "failed to build stream request", Cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/x-ndjson")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return &StreamError{Message: "failed to open mail stream", Cause: err}
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &AuthenticationError{StatusCode: response.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &StreamError{
			Message: fmt.Sprintf("unexpected stream status code: %d", response.StatusCode),
			Cause:   errors.New(strings.TrimSpace(string(body))),
		}
	}

	c.mu.Lock()
	c.activeResp = response.Body
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.activeResp == response.Body {
			c.activeResp = nil
		}
		c.mu.Unlock()
	}()

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerMaxToken)
	readCh := make(chan streamReadResult, 1)
	go func() {
		defer close(readCh)
		for scanner.Scan() {
			line := append([]byte(nil), bytes.TrimSpace(scanner.Bytes())...)
			readCh <- streamReadResult{line: line}
		}
		if err := scanner.Err(); err != nil {
			readCh <- streamReadResult{err: &StreamError{Message: "mail stream scanner error", Cause: err}}
			return
		}
		readCh <- streamReadResult{eof: true}
	}()

	idleTimer := time.NewTimer(c.socketTimeout)
	defer idleTimer.Stop()
	resetIdleTimer := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(c.socketTimeout)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idleTimer.C:
			_ = response.Body.Close()
			return &StreamError{Message: "mail stream stalled and will reconnect"}
		case result, ok := <-readCh:
			if !ok {
				return nil
			}
			if result.err != nil {
				return result.err
			}
			if result.eof {
				return nil
			}
			resetIdleTimer()
			if len(result.line) == 0 {
				continue
			}
			event := streamEvent{}
			if err := json.Unmarshal(result.line, &event); err != nil {
				return &StreamError{Message: "received invalid NDJSON event", Cause: err}
			}
			if event.Type == "ready" {
				if err := c.handleReadyEvent(event); err != nil {
					return err
				}
				continue
			}
			if event.Type == "heartbeat" {
				continue
			}
			if event.Type != "mail" {
				continue
			}
			message, err := parseMailEvent(event)
			if err != nil {
				return err
			}
			c.dispatchMessage(message)
		}
	}
}

func (c *Client) dispatchMessage(message MailMessage) {
	c.mu.RLock()
	fullListeners := make([]chan MailMessage, 0, len(c.allListeners))
	for _, ch := range c.allListeners {
		fullListeners = append(fullListeners, ch)
	}
	c.mu.RUnlock()

	for _, ch := range fullListeners {
		select {
		case ch <- message:
		default:
			c.dropped.Add(1)
		}
	}
	seenRecipients := map[string]struct{}{}
	for _, recipient := range message.Recipients {
		normalizedRecipient := strings.ToLower(strings.TrimSpace(recipient))
		if normalizedRecipient == "" {
			continue
		}
		if _, exists := seenRecipients[normalizedRecipient]; exists {
			continue
		}
		seenRecipients[normalizedRecipient] = struct{}{}
		perRecipient := message
		perRecipient.Address = normalizedRecipient
		for _, binding := range c.matchBindings(perRecipient.Address) {
			binding.mailbox.enqueue(perRecipient)
		}
	}
}

func (c *Client) matchBindings(address string) []*mailBinding {
	localPart, suffix, ok := splitAddress(address)
	if !ok {
		return nil
	}
	c.mu.RLock()
	chain := append([]*mailBinding(nil), c.bindings[suffix]...)
	if len(chain) == 0 {
		ownerUsername := strings.ToLower(strings.TrimSpace(c.ownerUsername))
		if ownerUsername != "" {
			rootSuffix := strings.ToLower(string(SuffixLinuxdoSpace))
			legacyNamespace := ownerUsername + "." + rootSuffix
			canonicalMailNamespace := ownerUsername + "-mail." + rootSuffix
			if suffix == legacyNamespace {
				chain = append([]*mailBinding(nil), c.bindings[canonicalMailNamespace]...)
			}
		}
	}
	c.mu.RUnlock()

	out := make([]*mailBinding, 0, len(chain))
	for _, binding := range chain {
		if !binding.matches(localPart) {
			continue
		}
		out = append(out, binding)
		if !binding.allowOverlap {
			break
		}
	}
	return out
}

func (c *Client) registerBinding(binding *mailBinding) error {
	c.mu.Lock()
	c.bindings[binding.suffix] = append(c.bindings[binding.suffix], binding)
	c.mu.Unlock()

	if err := c.syncRemoteMailboxFilters(true); err != nil {
		c.mu.Lock()
		chain := c.bindings[binding.suffix]
		filtered := chain[:0]
		for _, current := range chain {
			if current != binding {
				filtered = append(filtered, current)
			}
		}
		if len(filtered) == 0 {
			delete(c.bindings, binding.suffix)
		} else {
			c.bindings[binding.suffix] = filtered
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Client) unregisterMailbox(mailbox *Mailbox) {
	c.mu.Lock()
	for suffix, chain := range c.bindings {
		filtered := chain[:0]
		for _, binding := range chain {
			if binding.mailbox != mailbox {
				filtered = append(filtered, binding)
			}
		}
		if len(filtered) == 0 {
			delete(c.bindings, suffix)
			continue
		}
		c.bindings[suffix] = filtered
	}
	c.mu.Unlock()
	_ = c.syncRemoteMailboxFilters(false)
}

func (c *Client) handleReadyEvent(event streamEvent) error {
	ownerUsername := strings.ToLower(strings.TrimSpace(event.OwnerUsername))
	if ownerUsername == "" {
		return &StreamError{Message: "ready event did not include owner_username"}
	}

	c.mu.Lock()
	c.ownerUsername = ownerUsername
	c.mu.Unlock()
	if err := c.syncRemoteMailboxFilters(true); err != nil {
		return err
	}
	c.signalInitialReady(true, nil)
	return nil
}

func (c *Client) signalInitialReady(ok bool, err error) {
	c.mu.Lock()
	if c.initialReadyOK || c.initialErr != nil {
		c.mu.Unlock()
		return
	}
	if ok {
		c.initialReadyOK = true
	} else if err != nil {
		c.initialErr = err
	}
	ch := c.initialReadyCh
	c.initialReadyCh = nil
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (c *Client) resolveBindingSuffix(suffix any) (string, error) {
	switch current := suffix.(type) {
	case Suffix:
		normalizedSuffix := strings.ToLower(strings.TrimSpace(string(current)))
		if normalizedSuffix == "" {
			return "", errors.New("suffix must not be empty")
		}
		if normalizedSuffix != strings.ToLower(string(SuffixLinuxdoSpace)) {
			return normalizedSuffix, nil
		}
		ownerUsername := c.currentOwnerUsername()
		if ownerUsername == "" {
			return "", errors.New("stream bootstrap did not provide owner_username required to resolve SuffixLinuxdoSpace")
		}
		return ownerUsername + "-mail." + normalizedSuffix, nil
	case SemanticSuffix:
		normalizedBase := strings.ToLower(strings.TrimSpace(string(current.base)))
		if normalizedBase == "" {
			return "", errors.New("suffix must not be empty")
		}
		if normalizedBase != strings.ToLower(string(SuffixLinuxdoSpace)) {
			return normalizedBase, nil
		}
		ownerUsername := c.currentOwnerUsername()
		if ownerUsername == "" {
			return "", errors.New("stream bootstrap did not provide owner_username required to resolve SuffixLinuxdoSpace.WithSuffix(...)")
		}
		fragment, err := normalizeMailSuffixFragment(current.mailSuffixFragment)
		if err != nil {
			return "", err
		}
		return ownerUsername + "-mail" + fragment + "." + normalizedBase, nil
	case string:
		normalizedSuffix := strings.ToLower(strings.TrimSpace(current))
		if normalizedSuffix == "" {
			return "", errors.New("suffix must not be empty")
		}
		return normalizedSuffix, nil
	default:
		return "", fmt.Errorf("unsupported suffix type %T", suffix)
	}
}

func (c *Client) currentOwnerUsername() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.ToLower(strings.TrimSpace(c.ownerUsername))
}

func (c *Client) syncRemoteMailboxFilters(strict bool) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil
	}
	ownerUsername := strings.ToLower(strings.TrimSpace(c.ownerUsername))
	previous := slices.Clone(c.filterSuffixes)
	alreadySynced := c.filtersSynced
	c.mu.RUnlock()

	if ownerUsername == "" {
		return nil
	}

	fragments := c.collectRemoteMailboxSuffixFragments(ownerUsername)
	if len(fragments) == 0 && !alreadySynced {
		return nil
	}
	if alreadySynced && slices.Equal(previous, fragments) {
		return nil
	}

	payload, err := json.Marshal(map[string][]string{
		"suffixes": fragments,
	})
	if err != nil {
		if strict {
			return &StreamError{Message: "failed to encode remote mailbox filter sync payload", Cause: err}
		}
		return nil
	}

	requestContext, cancel := context.WithTimeout(context.Background(), c.connectTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPut, c.baseURL+streamFiltersPath, bytes.NewReader(payload))
	if err != nil {
		if strict {
			return &StreamError{Message: "failed to build remote mailbox filter sync request", Cause: err}
		}
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		if strict {
			return &StreamError{Message: "failed to synchronize remote mailbox filters", Cause: err}
		}
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if strict {
			return &StreamError{
				Message: fmt.Sprintf("unexpected mailbox filter sync status code: %d", response.StatusCode),
				Cause:   errors.New(strings.TrimSpace(string(body))),
			}
		}
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))

	c.mu.Lock()
	c.filtersSynced = true
	c.filterSuffixes = slices.Clone(fragments)
	c.mu.Unlock()
	return nil
}

func (c *Client) collectRemoteMailboxSuffixFragments(ownerUsername string) []string {
	rootSuffix := strings.ToLower(string(SuffixLinuxdoSpace))
	canonicalPrefix := ownerUsername + "-mail"
	fragmentsSet := map[string]struct{}{}

	c.mu.RLock()
	suffixes := make([]string, 0, len(c.bindings))
	for suffix := range c.bindings {
		suffixes = append(suffixes, suffix)
	}
	c.mu.RUnlock()

	for _, suffix := range suffixes {
		normalizedSuffix := strings.ToLower(strings.TrimSpace(suffix))
		if !strings.HasSuffix(normalizedSuffix, "."+rootSuffix) {
			continue
		}
		label := strings.TrimSuffix(normalizedSuffix, "."+rootSuffix)
		if strings.Contains(label, ".") || !strings.HasPrefix(label, canonicalPrefix) {
			continue
		}
		fragmentsSet[label[len(canonicalPrefix):]] = struct{}{}
	}

	fragments := make([]string, 0, len(fragmentsSet))
	for fragment := range fragmentsSet {
		fragments = append(fragments, fragment)
	}
	slices.Sort(fragments)
	return fragments
}

func (c *Client) isClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

func (c *Client) ensureUsable() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.fatalErr != nil {
		return c.fatalErr
	}
	if c.closed {
		return errors.New("client is closed")
	}
	return nil
}

func splitAddress(address string) (localPart string, suffix string, ok bool) {
	normalized := strings.ToLower(strings.TrimSpace(address))
	parts := strings.Split(normalized, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func normalizeBaseURL(baseURL string) (string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		return "", errors.New("base URL must not be empty")
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base URL must use http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("base URL must include hostname")
	}
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
	if parsed.Scheme == "http" && !isLocal {
		return "", errors.New("non-local base URL must use https")
	}
	return normalized, nil
}

func parseMailEvent(event streamEvent) (MailMessage, error) {
	encoded := strings.TrimSpace(event.RawMessageBase64)
	if encoded == "" {
		return MailMessage{}, &StreamError{Message: "mail event did not include raw_message_base64"}
	}
	rawBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return MailMessage{}, &StreamError{Message: "mail event contained invalid base64 message data", Cause: err}
	}
	receivedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(event.ReceivedAt))
	if err != nil {
		return MailMessage{}, &StreamError{Message: "invalid mail event timestamp", Cause: err}
	}

	parsed, parseErr := mail.ReadMessage(bytes.NewReader(rawBytes))
	headers := map[string]string{}
	var subject, messageID, fromHeader, toHeader, ccHeader, replyToHeader string
	var dateValue *time.Time
	var fromAddresses, toAddresses, ccAddresses, replyToAddresses []string
	var textBody, htmlBody string

	if parseErr == nil {
		subject = parsed.Header.Get("Subject")
		messageID = parsed.Header.Get("Message-Id")
		fromHeader = parsed.Header.Get("From")
		toHeader = parsed.Header.Get("To")
		ccHeader = parsed.Header.Get("Cc")
		replyToHeader = parsed.Header.Get("Reply-To")

		for key, values := range textproto.MIMEHeader(parsed.Header) {
			if len(values) == 0 {
				continue
			}
			headers[key] = values[0]
		}

		fromAddresses = parseAddressList(fromHeader)
		toAddresses = parseAddressList(toHeader)
		ccAddresses = parseAddressList(ccHeader)
		replyToAddresses = parseAddressList(replyToHeader)

		if dateRaw := parsed.Header.Get("Date"); dateRaw != "" {
			if parsedDate, dateErr := mail.ParseDate(dateRaw); dateErr == nil {
				dateCopy := parsedDate
				dateValue = &dateCopy
			}
		}
		bodyBytes, bodyErr := io.ReadAll(parsed.Body)
		if bodyErr == nil {
			textBody, htmlBody = extractBodies(parsed.Header.Get("Content-Type"), bodyBytes)
		}
	}

	recipients := normalizeRecipients(event.OriginalRecipients)
	primaryAddress := ""
	if len(recipients) > 0 {
		primaryAddress = recipients[0]
	}
	return MailMessage{
		Address:          primaryAddress,
		Sender:           strings.TrimSpace(event.OriginalEnvelopeFrom),
		Recipients:       append([]string(nil), recipients...),
		ReceivedAt:       receivedAt,
		Subject:          subject,
		MessageID:        messageID,
		Date:             dateValue,
		FromHeader:       fromHeader,
		ToHeader:         toHeader,
		CcHeader:         ccHeader,
		ReplyToHeader:    replyToHeader,
		FromAddresses:    append([]string(nil), fromAddresses...),
		ToAddresses:      append([]string(nil), toAddresses...),
		CcAddresses:      append([]string(nil), ccAddresses...),
		ReplyToAddresses: append([]string(nil), replyToAddresses...),
		Text:             textBody,
		HTML:             htmlBody,
		Headers:          copyStringMap(headers),
		Raw:              string(rawBytes),
		RawBytes:         append([]byte(nil), rawBytes...),
	}, nil
}

func normalizeRecipients(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		normalized := strings.ToLower(strings.TrimSpace(item))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func parseAddressList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	list, err := mail.ParseAddressList(raw)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		address := strings.ToLower(strings.TrimSpace(item.Address))
		if address == "" {
			continue
		}
		out = append(out, address)
	}
	return out
}

func extractBodies(contentType string, body []byte) (string, string) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.TrimSpace(string(body)), ""
	}

	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return strings.TrimSpace(string(body)), ""
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		var textParts []string
		var htmlParts []string
		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				break
			}
			payload, readErr := io.ReadAll(part)
			_ = part.Close()
			if readErr != nil {
				continue
			}
			partType, _, parseErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if parseErr != nil {
				partType = "text/plain"
			}
			switch strings.ToLower(partType) {
			case "text/plain":
				textParts = append(textParts, string(payload))
			case "text/html":
				htmlParts = append(htmlParts, string(payload))
			}
		}
		return strings.TrimSpace(strings.Join(textParts, "\n")), strings.TrimSpace(strings.Join(htmlParts, "\n"))
	}

	if strings.EqualFold(mediaType, "text/html") {
		return "", strings.TrimSpace(string(body))
	}
	return strings.TrimSpace(string(body)), ""
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
