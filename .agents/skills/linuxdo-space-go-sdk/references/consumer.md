# Consumer Guide

## Install

```bash
go get github.com/MoYeRanqianzhi/LinuxDoSpaceGoSDK
```

Import shape:

```go
import linuxdospace "github.com/MoYeRanqianzhi/LinuxDoSpaceGoSDK"
```

## Full stream

```go
client, err := linuxdospace.NewClient("lds_pat...")
if err != nil {
    panic(err)
}
defer client.Close()

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

all, unlisten := client.Listen(ctx)
defer unlisten()

for item := range all {
    fmt.Println(item.Address, item.Subject)
}
```

## Mailbox binding

```go
mailbox, err := client.BindExact("alice", linuxdospace.SuffixLinuxdoSpace, false)
if err != nil {
    panic(err)
}
defer mailbox.Close()

ch, err := mailbox.Listen(ctx)
if err != nil {
    panic(err)
}

for item := range ch {
    fmt.Println(item.Address, item.Subject)
}
```

## Key semantics

- `Err()` exposes fatal stream failure after listeners close.
- `Dropped()` exposes backpressure drops.
- `Route(message)` is local matching only.

