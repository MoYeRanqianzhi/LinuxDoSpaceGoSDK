# Task Templates

## Create one client

```go
client, err := linuxdospace.NewClient(
    token,
    linuxdospace.WithBaseURL("https://api.linuxdo.space"),
)
```

## Bind one catch-all

```go
mailbox, err := client.BindPattern(".*", linuxdospace.SuffixLinuxdoSpace, true)
```

## Route one message locally

```go
targets := client.Route(message)
```

## Check terminal state

```go
if err := client.Err(); err != nil {
    log.Println(err)
}
```
