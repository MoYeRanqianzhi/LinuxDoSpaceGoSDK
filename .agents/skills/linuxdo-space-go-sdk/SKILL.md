---
name: linuxdo-space-go-sdk
description: Use when writing or fixing Go code that consumes or maintains the LinuxDoSpace Go SDK under `sdk/go`, especially for module integration, `Client` construction, full-stream listeners, mailbox bindings, ordered matching, `allowOverlap` behavior, lifecycle/error handling, release guidance, and local validation.
---

# LinuxDoSpace Go SDK

Read [references/consumer.md](references/consumer.md) first for normal SDK usage.
Read [references/api.md](references/api.md) for exact public Go API names.
Read [references/examples.md](references/examples.md) for task-shaped snippets.
Read [references/development.md](references/development.md) only when editing `sdk/go`.

## Workflow

1. Prefer the public module path `github.com/MoYeRanqianzhi/LinuxDoSpaceGoSDK`.
2. The SDK root relative to this `SKILL.md` is `../../../`.
3. Preserve these invariants:
   - one `Client` owns one upstream HTTPS stream
   - `Listen(ctx)` is the full-stream consumer entrypoint
   - `BindExact(...)` / `BindPattern(...)` register mailbox bindings locally
   - mailbox queues activate only while mailbox listen is active
   - `SuffixLinuxdoSpace` is semantic and resolves after `ready.owner_username`
   - exact and regex bindings share one ordered chain per suffix
   - `allowOverlap=false` stops at the first match; `true` continues
   - remote `BaseURL` must use `https://`
4. Keep README, code, tests, and workflows aligned when behavior changes.
5. Validate with the commands in `references/development.md`.

## Do Not Regress

- Do not introduce per-mailbox upstream connections.
- Do not document hidden pre-listen mailbox buffering.
- Do not describe install as `go install`; this is a library module.
