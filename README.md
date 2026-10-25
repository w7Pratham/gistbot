# Zero Two (02)

Zero Two — short **02** — is a private Telegram bot that runs headless
[OpenCode](https://opencode.ai) jobs against GitHub repositories and returns a
gist-style report. Technical identifiers stay `gistbot` for the binary, Unix
user, paths, state files, and service units.

## Status

The repository is at the M0/proof and configuration stage. The Telegram client,
job manager, runners, and bot loop are implemented in later milestones. The
config package and build skeleton are in place.

## Requirements

- Go 1.25 or newer
- Standard library only — no third-party Go dependencies
- An OpenCode installation and a Telegram bot token

## Layout

```text
.
├── AGENTS.md              # security and contribution rules
├── Makefile               # fixed build/test commands
├── docs/                  # design and M0 evidence
├── internal/config/       # typed, validated environment configuration
└── scripts/               # M0 setup and proof scripts
```

## Configuration

Configuration comes from environment variables supplied by systemd
`EnvironmentFile`. Copy `.env.example` to `.env` for local use and fill in the
secrets. Never commit `.env`.

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

## Commands

```bash
make build      # go build ./...
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
