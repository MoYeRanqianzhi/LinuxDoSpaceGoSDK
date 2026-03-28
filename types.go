package linuxdospace

import (
	"errors"
	"strings"
	"time"
)

// Suffix is a typed mailbox domain suffix to avoid typo-prone plain strings.
type Suffix string

// SemanticSuffix is one first-party semantic mailbox suffix with an optional
// dynamic `-mail<fragment>` extension.
//
// The plain semantic root still renders as `linuxdo.space`, but SDK bind calls
// resolve it against the current token owner after the stream `ready` event:
//
// - `SuffixLinuxdoSpace` -> `<owner_username>-mail.linuxdo.space`
// - `SuffixLinuxdoSpace.WithSuffix("foo")` -> `<owner_username>-mailfoo.linuxdo.space`
type SemanticSuffix struct {
	base               Suffix
	mailSuffixFragment string
}

const (
	// SuffixLinuxdoSpace is the first-party LinuxDoSpace namespace suffix.
	//
	// This value is semantic rather than literal: SDK bindings resolve it to the
	// current token owner's canonical mail namespace under `linuxdo.space`.
	// Current versions automatically keep compatibility with both:
	//
	// - `<owner_username>-mail.linuxdo.space`
	// - legacy incoming mail addressed to `<owner_username>.linuxdo.space`
	SuffixLinuxdoSpace Suffix = "linuxdo.space"
)

// WithSuffix returns the same semantic base with one normalized dynamic mail
// suffix fragment, for example `SuffixLinuxdoSpace.WithSuffix("foo")`.
func (s Suffix) WithSuffix(fragment string) SemanticSuffix {
	return SemanticSuffix{
		base:               s,
		mailSuffixFragment: fragment,
	}
}

// MailMessage is one parsed mail event projected for SDK consumers.
//
// The struct mirrors the protocol contract and exposes parsed header/body fields.
// When some source data is unavailable, fields remain empty-value rather than
// inventing synthetic values.
type MailMessage struct {
	Address    string
	Sender     string
	Recipients []string
	ReceivedAt time.Time

	Subject   string
	MessageID string
	Date      *time.Time

	FromHeader    string
	ToHeader      string
	CcHeader      string
	ReplyToHeader string

	FromAddresses    []string
	ToAddresses      []string
	CcAddresses      []string
	ReplyToAddresses []string

	Text string
	HTML string

	Headers map[string]string

	Raw      string
	RawBytes []byte
}

func normalizeMailSuffixFragment(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", nil
	}

	normalized := make([]rune, 0, len(value))
	lastWasDash := false
	for _, character := range value {
		isAlpha := character >= 'a' && character <= 'z'
		isDigit := character >= '0' && character <= '9'
		if isAlpha || isDigit {
			normalized = append(normalized, character)
			lastWasDash = false
			continue
		}
		if !lastWasDash {
			normalized = append(normalized, '-')
			lastWasDash = true
		}
	}

	trimmed := strings.Trim(string(normalized), "-")
	if trimmed == "" {
		return "", errors.New("mail suffix fragment does not contain any valid dns characters")
	}
	if strings.Contains(trimmed, ".") {
		return "", errors.New("mail suffix fragment must stay inside one dns label")
	}
	if len(trimmed) > 48 {
		return "", errors.New("mail suffix fragment must be 48 characters or fewer")
	}
	return trimmed, nil
}
