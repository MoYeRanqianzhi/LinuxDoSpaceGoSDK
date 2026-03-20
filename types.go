package linuxdospace

import "time"

// Suffix is a typed mailbox domain suffix to avoid typo-prone plain strings.
type Suffix string

const (
	// SuffixLinuxdoSpace is the first-party LinuxDoSpace namespace suffix.
	//
	// This value is semantic rather than literal: SDK bindings resolve it to the
	// current token owner's namespace suffix `<owner_username>.linuxdo.space`
	// after the stream handshake receives `ready.owner_username`.
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
