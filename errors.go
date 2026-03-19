package linuxdospace

import "fmt"

// AuthenticationError reports that the backend rejected the token.
type AuthenticationError struct {
	// StatusCode is the HTTP status returned by the stream endpoint.
	StatusCode int
	// Message stores the human-readable backend failure detail when available.
	Message string
}

// Error implements the error interface.
func (e *AuthenticationError) Error() string {
	if e == nil {
		return "linuxdospace authentication error"
	}
	if e.Message == "" {
		return fmt.Sprintf("linuxdospace authentication failed (status=%d)", e.StatusCode)
	}
	return fmt.Sprintf("linuxdospace authentication failed (status=%d): %s", e.StatusCode, e.Message)
}

// StreamError reports transport/protocol/parsing failures in the mail stream.
type StreamError struct {
	// Message describes the high-level stream failure.
	Message string
	// Cause contains the wrapped lower-level error.
	Cause error
}

// Error implements the error interface.
func (e *StreamError) Error() string {
	if e == nil {
		return "linuxdospace stream error"
	}
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

// Unwrap allows errors.Is/errors.As to inspect the underlying cause.
func (e *StreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
