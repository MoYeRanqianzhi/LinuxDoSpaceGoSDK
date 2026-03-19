# LinuxDoSpace Go SDK

`linuxdospace-go` is the Go SDK for LinuxDoSpace token mail streaming.

This SDK follows `sdk/spec/MAIL_STREAM_PROTOCOL.md` and the current Python SDK semantics:

- one `Client` keeps one upstream stream to `/v1/token/email/stream`
- `Listen` receives all mail events for the token
- mailbox bindings are local only (server does not know local rules)
- exact and regex bindings share one ordered chain per suffix
- `AllowOverlap=false` stops on first match
- `AllowOverlap=true` continues matching later bindings
- mailbox queues activate only while mailbox listen is active

## Install

```bash
go get github.com/MoYeRanqianzhi/LinuxDoSpace/sdk/go
```

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"time"

	linuxdospace "github.com/MoYeRanqianzhi/LinuxDoSpace/sdk/go"
)

func main() {
	client, err := linuxdospace.NewClient(
		"your_api_token",
		linuxdospace.WithBaseURL("https://api.linuxdo.space"),
	)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	all, unlisten := client.Listen(ctx)
	defer unlisten()

	for item := range all {
		fmt.Println(item.Address, item.Sender, item.Subject)
	}
}
```

## Mailbox binding

```go
mailbox, err := client.BindExact("alice", linuxdospace.SuffixLinuxdoSpace, false)
if err != nil {
	panic(err)
}
defer mailbox.Close()

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

ch, err := mailbox.Listen(ctx)
if err != nil {
	panic(err)
}

for mail := range ch {
	fmt.Println(mail.Address, mail.Subject)
}
```

## Public API

- `NewClient(token string, options ...Option) (*Client, error)`
- `(*Client).Listen(ctx context.Context) (<-chan MailMessage, func())`
- `(*Client).BindExact(prefix string, suffix Suffix, allowOverlap bool) (*Mailbox, error)`
- `(*Client).BindPattern(pattern string, suffix Suffix, allowOverlap bool) (*Mailbox, error)`
- `(*Client).Route(message MailMessage) []*Mailbox`
- `(*Client).Close() error`
- `(*Mailbox).Listen(ctx context.Context) (<-chan MailMessage, error)`
- `(*Mailbox).Close()`

## Error types

- `*AuthenticationError`
- `*StreamError`

These both implement `error`.
