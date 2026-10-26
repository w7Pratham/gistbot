# Zero Two (02)

Zero Two — short **02** — is a private Telegram bot that runs headless
[OpenCode](https://opencode.ai) jobs against GitHub repositories and returns a
gist-style report. Technical identifiers stay `gistbot` for the binary, Unix
user, paths, state files, and service units.

## Status

Zero Two is implemented: the owner-gated Telegram client and long-poll loop,
the job manager and queue, the CLI and API runners, repository cloning,
persisted model and runner selection, document browsing, the compact card UI,
and the systemd deployment are all in place. The automated suite runs with
`make test` and `make test-race`; the end-to-end tests in `cmd/gistbot` pin the
one-card, one-album success shape and the failure tail plus `.log` attachment.

The remaining step is manual acceptance on a phone against the live bot. The
checklist in `docs/install.md` has **not** been performed in this checkout;
record the date and result next to each item when it is.

## Requirements

- Go 1.25 or newer
- Standard library only — no third-party Go dependencies
- An OpenCode installation and a Telegram bot token

## Layout

```text
.
├── AGENTS.md              # security and contribution rules
├── Makefile               # fixed build/test commands
├── cmd/gistbot/           # entry point: wiring and graceful shutdown
├── internal/
│   ├── bot/               # command dispatch, rendering, document browsing
│   ├── config/            # typed, validated environment configuration
│   ├── jobs/              # job manager, queue, CLI and API runners
│   ├── opencode/          # loopback OpenCode API client and model listing
│   ├── repo/              # GitHub URL validation, cloning, gist documents
│   ├── state/             # persisted model, runner, and job state
│   └── telegram/          # Telegram client and long-poll loop
├── deploy/                # systemd user units
├── docs/                  # install/operations guide and M0 evidence
└── scripts/               # M0 setup and proof scripts
```

Tests live beside their packages as `*_test.go` files, with fixture scripts
under `internal/*/testdata/`.

## Configuration

Configuration comes from process environment variables. In production, systemd
supplies `/home/gistbot/.config/gistbot/gistbot.env` via `EnvironmentFile`. The
binary does **not** auto-load `.env`; for a foreground run, source the
`gistbot.env` file first. Use `.env.example` as the template. Never commit env
files.

Required:

| Variable | Meaning |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | Telegram bot token |
| `OWNER_USER_ID` | Telegram user ID allowed to use the bot |
| `WORK_DIR` | Absolute job sandbox directory (not `/`) |
| `OPENCODE_BIN` | Absolute path to the OpenCode executable |
| `OPENCODE_API_PASSWORD` | Password for the loopback OpenCode API |
| `MODEL_FREE` | Free model reference |
| `MODEL_FLASH` | Default flash model reference |
| `MODEL_PRO` | Pro model reference |
| `STATE_FILE` | Absolute path to persisted state |
| `LOG_DIR` | Absolute log directory |

Optional defaults:

| Variable | Default |
| --- | --- |
| `TELEGRAM_API_URL` | `https://api.telegram.org` |
| `OPENCODE_API_URL` | `http://127.0.0.1:4096` |
| `OPENCODE_API_UNIT` | `opencode-gist.service` |
| `DEFAULT_MODEL` | `flash` |
| `DEFAULT_RUNNER` | `cli` |
| `JOB_TIMEOUT` | `20m` |
| `CLONE_TIMEOUT` | `5m` |
| `MAX_QUEUE` | `3` (1–10) |
| `ALLOWED_HOSTS` | `github.com` (only value accepted) |

Model references use `provider/model` or `provider/model#variant`. The bot
resolves the `free` alias to the configured free model unless a per-job free
override is supplied.

`config.Load` reads only the getter it is given, so tests and the production
systemd environment behave the same way. Errors name the offending variable but
never print its value.

## Telegram UI

One job owns one card. It is sent when `/gist` is accepted (`⚡ Running`, or
`🕓 Queued #n` when work is already in progress), edited to `⚡ Running` when
the worker starts it, and edited once more to its terminal state. A successful
job never sends a second summary message.

- **Success:** the final card shows the duration and document count, then the
  three gist documents arrive as one media group in canonical order
  (`codemap.md`, `routing.md`, `observation.md`). A partial run reports the
  missing documents on the card; a single document is sent alone, and an album
  rejected by Telegram falls back to paced separate documents.
- **Failure, cancel, or timeout:** the same card is edited with the status,
  error, duration, and the newest log lines in a short `<pre>` tail. Exactly one
  `<job>.log` attachment follows (capped at the most recent 5 MiB). No album is
  sent.
- **Command menu:** `/gist`, `/status`, `/cancel`, `/list`, `/show`, `/log`,
  `/model`, `/runner`, and `/help` are registered once after the webhook is
  deleted and before long polling starts. A menu failure is logged and does not
  stop the bot.
- **Browsing:** `/status` shows only the running job and queue. `/list` is
  newest first and bounded to one Telegram message. `/show` with no arguments
  resends the newest installed repository's documents; `/show repo doc` sends
  one document. `/log` returns a compact tail card plus the `.log` file.
- **No buttons:** the UI uses text commands only; there are no inline keyboards
  or callback queries.

## Commands

```bash
make build      # build bin/gistbot from ./cmd/gistbot
make test       # go test ./...
make vet        # go vet ./...
make test-race  # go test -race ./...
```

Focused config run:

```bash
go test ./internal/config -v
```

## Security

- Never commit `.env`, tokens, passwords, or generated state.
- Only `github.com` is accepted as a clone host.
- Jobs run in `WORK_DIR` with per-job sandboxes; no arbitrary shell execution.
- OpenCode API access is loopback-only with HTTP Basic authentication.

See `AGENTS.md` for the full checklist and `docs/m0.md` for proof-of-concept
evidence.
