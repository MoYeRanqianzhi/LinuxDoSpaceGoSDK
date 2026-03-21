# API Reference

## Paths

- SDK root: `../../../`
- Module metadata: `../../../go.mod`
- Core implementation: `../../../client.go`
- Types: `../../../types.go`
- Errors: `../../../errors.go`
- Consumer README: `../../../README.md`

## Public surface

- Construction / options:
  - `NewClient(...)`
  - `WithBaseURL(...)`
  - `WithConnectTimeout(...)`
  - `WithSocketTimeout(...)`
  - `WithReconnectDelay(...)`
  - `WithHTTPClient(...)`
- Client:
  - `Listen(ctx)`
  - `BindExact(...)`
  - `BindPattern(...)`
  - `Route(message)`
  - `Err()`
  - `Dropped()`
  - `Close()`
- Mailbox:
  - `Listen(ctx)`
  - `Err()`
  - `Dropped()`
  - `Close()`
  - attribute-style methods such as `Mode()`, `Suffix()`, `Pattern()`, `Address()`

## Semantics

- `Listen(ctx)` returns one channel plus an `unlisten` function.
- `Mailbox.Listen(ctx)` returns one channel and activates mailbox buffering only while active.
- Regex bindings use full-match semantics.
- `SuffixLinuxdoSpace` is semantic, not literal.
