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
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the production API origin.
	DefaultBaseURL = "https://api.linuxdo.space"
	// streamPath is the protocol-defined stream endpoint path.
	streamPath = "/v1/token/email/stream"

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
	httpClient     *http.Client
	reconnectDelay time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu             sync.RWMutex
	closed         bool
	fatalErr       error
	allListeners   map[int]chan MailMessage
	nextListenerID int
	bindings       map[string][]*mailBinding
	activeResp     io.Closer
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

	OriginalEnvelopeFrom string   `json:"original_envelope_from"`
	OriginalRecipients   []string `json:"original_recipients"`
	ReceivedAt           string   `json:"received_at"`
	RawMessageBase64     string   `json:"raw_message_base64"`
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
		httpClient:     httpClient,
		reconnectDelay: cfg.reconnectDelay,
		ctx:            ctx,
		cancel:         cancel,
		allListeners:   map[int]chan MailMessage{},
		bindings:       map[string][]*mailBinding{},
	}

	// Constructor must perform one immediate connection attempt.
	initialCtx, initialCancel := context.WithTimeout(ctx, cfg.connectTimeout)
	initialErr := client.initialConnect(initialCtx)
	initialCancel()
	if initialErr != nil {
		cancel()
		return nil, initialErr
	}

	client.wg.Add(1)
	go client.streamLoop()
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

// BindExact binds one exact prefix+suffix mailbox rule.
func (c *Client) BindExact(prefix string, suffix Suffix, allowOverlap bool) (*Mailbox, error) {
	if err := c.ensureUsable(); err != nil {
		return nil, err
	}
	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	normalizedSuffix := strings.ToLower(strings.TrimSpace(string(suffix)))
	if normalizedPrefix == "" {
		return nil, errors.New("prefix must not be empty")
	}
	if normalizedSuffix == "" {
		return nil, errors.New("suffix must not be empty")
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
	c.registerBinding(binding)
	return mailbox, nil
}

// BindPattern binds one regex+suffix mailbox rule.
func (c *Client) BindPattern(pattern string, suffix Suffix, allowOverlap bool) (*Mailbox, error) {
	if err := c.ensureUsable(); err != nil {
		return nil, err
	}
	normalizedPattern := strings.TrimSpace(pattern)
	normalizedSuffix := strings.ToLower(strings.TrimSpace(string(suffix)))
	if normalizedPattern == "" {
		return nil, errors.New("pattern must not be empty")
	}
	if normalizedSuffix == "" {
		return nil, errors.New("suffix must not be empty")
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
	c.registerBinding(binding)
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
	for {
		if c.isClosed() {
			return
		}
		err := c.consumeOnce(c.ctx)
		if err == nil || c.isClosed() {
			return
		}
		var authErr *AuthenticationError
		if errors.As(err, &authErr) {
			c.failFatal(err)
			return
		}

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

// initialConnect validates token/auth/stream contract during constructor.
//
// It intentionally reads only enough data to prove the stream is healthy, then
// returns so NewClient does not block on an endless stream.
func (c *Client) initialConnect(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+streamPath, nil)
	if err != nil {
		return &StreamError{Message: "failed to build initial stream request", Cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/x-ndjson")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return &StreamError{Message: "failed to open initial mail stream", Cause: err}
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

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 8*1024), 256*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		event := streamEvent{}
		if err := json.Unmarshal(line, &event); err != nil {
			return &StreamError{Message: "received invalid NDJSON event during initial connect", Cause: err}
		}
		// Any valid event means the stream is alive and protocol is working.
		return nil
	}
	if err := scanner.Err(); err != nil {
		return &StreamError{Message: "initial mail stream scanner error", Cause: err}
	}
	return &StreamError{Message: "initial mail stream ended before handshake event"}
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

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		event := streamEvent{}
		if err := json.Unmarshal(line, &event); err != nil {
			return &StreamError{Message: "received invalid NDJSON event", Cause: err}
		}
		if event.Type == "ready" || event.Type == "heartbeat" {
			continue
		}
		if event.Type != "mail" {
			continue
		}
		messages, err := parseMailEvent(event)
		if err != nil {
			return err
		}
		for _, message := range messages {
			c.dispatchMessage(message)
		}
	}
	if err := scanner.Err(); err != nil {
		return &StreamError{Message: "mail stream scanner error", Cause: err}
	}
	return nil
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
		}
	}
	for _, binding := range c.matchBindings(message.Address) {
		binding.mailbox.enqueue(message)
	}
}

func (c *Client) matchBindings(address string) []*mailBinding {
	localPart, suffix, ok := splitAddress(address)
	if !ok {
		return nil
	}
	c.mu.RLock()
	chain := append([]*mailBinding(nil), c.bindings[suffix]...)
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

func (c *Client) registerBinding(binding *mailBinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bindings[binding.suffix] = append(c.bindings[binding.suffix], binding)
}

func (c *Client) unregisterMailbox(mailbox *Mailbox) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

func parseMailEvent(event streamEvent) ([]MailMessage, error) {
	encoded := strings.TrimSpace(event.RawMessageBase64)
	if encoded == "" {
		return nil, &StreamError{Message: "mail event did not include raw_message_base64"}
	}
	rawBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, &StreamError{Message: "mail event contained invalid base64 message data", Cause: err}
	}
	receivedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(event.ReceivedAt))
	if err != nil {
		return nil, &StreamError{Message: "invalid mail event timestamp", Cause: err}
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
	if len(recipients) == 0 {
		recipients = []string{""}
	}

	out := make([]MailMessage, 0, len(recipients))
	for _, recipient := range recipients {
		out = append(out, MailMessage{
			Address:          recipient,
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
		})
	}
	return out, nil
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
