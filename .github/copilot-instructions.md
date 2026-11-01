# Copilot Instructions

Read [AGENTS.md](../AGENTS.md) for repository conventions and ownership boundaries.
Use [README.md](../README.md) for setup, configuration, CLI usage, and operational
details. These instructions supplement those documents with durable decisions
from the project's design discussions. Do not revive superseded plans.

## Domain And Architecture

- gmail-poppers replaces Gmail's external-account POP3 fetching, not Gmail's own
  IMAP server. It fetches external POP3/IMAP mail and imports it into Gmail.
- This is a practical personal home-lab tool, deployed through Docker/Portainer,
  including Raspberry Pi. Prefer a small service over a general-purpose platform.
- Support multiple Gmail accounts, each with multiple named sources. Preserve
  map-based YAML configuration and `${VAR}` expansion for Portainer-managed secrets.
- Preserve unread inbox imports with an optional per-source Gmail label, IMAP
  folder lists defaulting to INBOX, per-source intervals, and configurable retention.
- Preserve UID/UIDL-based progress, IMAP UIDVALIDITY handling, and safe first-run
  behavior (`initial_sync: new_only` by default). Never delete baseline mail or
  mail that has not been successfully imported. Do not promise exactly-once
  delivery across Gmail and local state files.
- Keep Gmail API import as the default and IMAP APPEND as an alternative. Preserve
  the README warning about IMAP APPEND bypassing Gmail spam scanning.
- Use one binary: no arguments runs the service; `add target` performs local
  OAuth and target setup, `add source` defines source connections, `link` creates
  routes, and `test` checks a configured source or target connection.
- Renew existing API target authorization with `add target --reauthorize`,
  preserving configuration and routes. Non-interactive setup must never launch
  OAuth or prompt; it requires existing tokens for API targets.
- OAuth authorization is CLI-only, on the user's browser computer. Preserve the
  temporary loopback callback, PKCE, state validation, and Gmail account matching.
  Store per-account tokens separately from YAML; support TOKEN_DIR and automatic
  refresh by the service. Do not add OAuth forms or interactive HTTP inputs.
- The service's HTTP interface is read-only health/status reporting. Gatus mode
  is selected through environment variables, not mailbox configuration. Preserve
  per-source results, actionable error messages, and push heartbeat support.

## Delegate By Responsibility

When relevant work is substantial, use the following custom agents as subagents
by their exact names. The main agent owns production-code changes and integration.

| Agent | Delegate |
|---|---|
| [README Maintainer](agents/readme.agent.md) | README updates and verification against implemented behavior |
| [CI-CD Maintainer](agents/ci-cd.agent.md) | GitHub Actions, quality gates, Docker builds, and GHCR configuration |
| [Test Runner](agents/test-runner.agent.md) | Existing tests and checks; no code edits or commits |
| [Test Writer](agents/test-writer.agent.md) | Focused regression/unit tests; no production-code edits |

- Do not delegate trivial edits merely to involve an agent. If the required agent
  is unavailable, state that and perform the bounded task yourself; do not claim
  delegation occurred.
- Give each subagent the exact task, permitted files, relevant behavior, and
  validation criteria. Pass current user constraints and existing-worktree context.
- Parallelize only independent tasks with non-overlapping writes. Run validation
  after dependent edits finish. Specialists must not recursively delegate.
- When coordinating multiple specialists, explicitly ask them to leave changes
  uncommitted. The main agent reviews, integrates, verifies, and commits them.
- Keep a failing regression test meaningful. Have the main agent repair the
  owning production code rather than asking Test Writer to weaken the assertion.

## Safety And Scope

- Everything committed to this repository is English, including agent files,
  comments, logs, errors, examples, and documentation. Match conversation language
  to the user when appropriate.
- Never log or display passwords, app passwords, OAuth tokens, or client secrets.
  Do not inspect private configuration or secret files to discover credentials.
  Never read `~/.config/gh/hosts.yml`; if authorized GitHub work needs a token,
  obtain it with `gh auth token` in-process without printing its value.
- Verify TLS certificates and never silently fall back to plaintext; plaintext
  requires explicit `tls: none` configuration.
- DRY_RUN must not write to mail servers, Gmail mailboxes, or state files. Cover
  every new write path with a read-only/dry-run regression test.
- CLI connection checks perform authentication and simple read-only operations,
  without message bodies, polling, Gatus pushes, or state/token persistence.
  OAuth refresh for these checks stays in memory.
- Run real-server checks or OAuth authorization only when explicitly requested
  with user-provided account selection and paths. Routine tests use fake servers,
  synthetic credentials, and temporary directories.
- Keep dependencies lean. Reuse protocol/OAuth libraries and standard-library
  HTTP/JSON APIs; do not add OpenTelemetry, gRPC, or the generated Google SDK just
  for simple Gmail REST calls or Gatus reporting. Use the maintained YAML package.
- Keep the container lightweight and nonroot. Preserve amd64, arm64, and arm/v7
  support, quality gates before builds, and PR builds without registry publishing.
- Make scoped, testable changes; reuse nearby code/tests rather than adding
  unnecessary files or abstractions. Fix root causes, not unrelated issues.
- Add comments only for facts the code cannot express, in one short line. Do not
  create Markdown change reports unless requested. Keep README current for
  relevant user-facing changes, without redundant history or absent-feature lists.
- Preserve the README's vibe-coded AI disclosure and MIT license. Use `konqi` as
  the public author identity, not a real name or employer address.

## Verification And Git

- Use Go via goenv and the repository's .go-version; do not replace the user's
  toolchain as an incidental task. Run a focused test immediately after a code edit,
  then expand verification according to risk and the requested scope.
- Before each commit, format changed Go files, ensure `gofmt -l .` is empty, and
  run `go vet ./...` and `go test ./...`. Follow the existing CI commands for race,
  lint, vulnerability, and build checks when relevant. Report blockers honestly.
- Commit after each substantial, verified change with a meaningful English gitmoji
  message. Verify repo-local identity `konqi <github@konqi.de>`; never silently use
  the global real-name/work-email identity or alter global Git configuration.
- Preserve existing user changes; stage only your own changes and review the
  formatted/staged diff. Never use destructive checkout/reset to compare revisions;
  use a separate worktree for historical build comparisons.
- Do not push, publish images, deploy, change remote settings, or rewrite history
  without explicit authorization. A requested squash/amend must preserve contents.
- Distinguish local tests and simulated servers from real-account verification,
  actual GitHub Actions runs, and successful registry publication.