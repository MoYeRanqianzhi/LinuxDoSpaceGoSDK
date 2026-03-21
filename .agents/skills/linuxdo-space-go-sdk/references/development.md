# Development Guide

## Workdir

```bash
cd sdk/go
```

## Validate

```bash
gofmt -l .
go test ./...
```

Optional focused checks:

```bash
go test -run TestOrderedMatchingAndAllowOverlap ./...
go test -run TestMailboxNoBackfillBeforeListen ./...
```

## Release model

- Workflow file: `../../../.github/workflows/release.yml`
- Trigger: push tag `v*`
- Current release output is a source archive uploaded to GitHub Release

## Keep aligned

- `../../../client.go`
- `../../../types.go`
- `../../../errors.go`
- `../../../README.md`
- `../../../client_test.go`
- `../../../.github/workflows/ci.yml`
- `../../../.github/workflows/release.yml`
