package linuxdospace

import "time"

// Suffix is a typed mailbox domain suffix to avoid typo-prone plain strings.
type Suffix string

const (
	// SuffixLinuxdoSpace is the current first-party suffix.
	SuffixLinuxdoSpace Suffix = "linuxdo.space"
)

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
