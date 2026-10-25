# AGENTS.md

Guidelines for humans and coding agents working on this repository.

## Language

Everything in this repository is written in **English**: code, comments, log
messages, error messages, web UI texts, examples and documentation.

## Project layout

| Path | Purpose |
|---|---|
| `cmd/gmail-poppers` | Entry point: wiring, signal handling |
| `internal/config` | YAML config loading, `${VAR}` expansion, validation |
| `internal/mailconn` | TCP/TLS dialing incl. TLS auto-detection |
| `internal/source/pop3` | POP3 source |
| `internal/source/imap` | IMAP source |
| `internal/state` | Per-source JSON state (seen UIDs, failure counters) |
| `internal/sink/gmailapi` | Gmail API `messages.import` sink (OAuth) |
| `internal/sink/gmailimap` | Gmail IMAP `APPEND` sink (app password) |
| `internal/runner` | Per-source polling loop |
| `internal/status` | In-memory status registry |
| `internal/gatus` | Gatus push client and endpoint snippet rendering |
| `internal/web` | HTTP server: status endpoints, OAuth bootstrap UI |

## Conventions

- Run `gofmt -l .`, `go vet ./...` and `go test ./...` before every commit.
- Error messages carry a short context prefix (e.g. `pop3 login: ...`,
  `gmail api: ...`) because they are shown verbatim in Gatus.
- Never log passwords, app passwords or OAuth tokens.
- Never fall back to plaintext connections unless `tls: none` is configured.
- Commit messages use [gitmoji](https://gitmoji.dev/) and English, e.g.
  `✨ Add IMAP source`.
- Comments only where the code cannot speak for itself; keep them short.
