# Gistbot

Gistbot — an owner-only Telegram bot that turns a GitHub repository into three gist documents with OpenCode.

It is self-hosted, owner-only, and pre-1.0. There is no multi-user support and no hosted service.

## What it does

- Send `/gist <url> [focus]` to clone a public GitHub repository and analyze it with OpenCode. An optional focus query can be appended to guide the inspection.
- **One job card:** A single compact message tracks each job (`⚡ Running` or `🕓 Queued #n`) and updates in place as the worker processes the repository.
- **One media group:** When the job succeeds, the job card updates with the run duration and document count, and the bot sends the three generated gist documents (`codemap.md`, `routing.md`, and `observation.md`) as a single Telegram media group (album) in canonical order.
- **Failure log:** If a job fails, times out, or is cancelled, the card is edited to show the status, error message, elapsed time, and a `<pre>` tail of recent log output. The bot then sends exactly one `<job>.log` file attachment (capped to the most recent 5 MiB). No media album is sent.

## Why it exists

Gistbot provides quick repository orientation without opening an editor. When you discover or want to evaluate a new codebase from your phone or mobile device, Gistbot handles cloning, agent execution, and document delivery directly inside Telegram so you can understand the project's layout, routing, and behavior in seconds.

## Architecture

```text
Telegram owner → owner gate → single-worker queue → fresh git clone → OpenCode repo-scout (cli|api) → docs → card + album
```

1. **Telegram owner:** The authorized user sends bot commands from Telegram.
2. **Owner gate:** Validates incoming updates against `OWNER_USER_ID`. Messages from any other user are dropped.
3. **Single-worker queue:** Serializes jobs in a bounded FIFO queue (capacity 1–10).
4. **Fresh git clone:** Clones the repository cleanly into an isolated subdirectory within `WORK_DIR`. Hostname is restricted strictly to `github.com`.
5. **OpenCode repo-scout:** Runs the `repo-scout` agent via headless CLI (`opencode run`) or the loopback HTTP API to generate `codemap.md`, `routing.md`, and `observation.md`.
6. **Docs:** Validates document existence, sanitizes links, and prepares delivery payloads.
7. **Card + album:** Updates the existing Telegram job card and sends the document album.

**Note:** The bot contains no LLM. Gistbot is a Go coordinator; all language model interactions are performed by OpenCode using your configured model provider.

## Requirements

- Go 1.25 or newer (standard library only; no third-party Go dependencies)
- [OpenCode](https://opencode.ai) installed and authenticated with your model provider
- A Telegram bot token from [@BotFather](https://t.me/BotFather)
- Linux with systemd (for the provided user service units)
- A model provider account (model usage may incur API costs from your provider)

## Quick start

1. **Build the binary:**
   ```bash
   make build
   ```
   This builds `bin/gistbot` from `./cmd/gistbot`.

2. **Configure environment:**
   Copy [.env.example](.env.example) to configure your deployment. See [docs/install.md](docs/install.md) for full setup instructions or use [scripts/host-install.sh](scripts/host-install.sh). Two environment files (mode `0600`, owned by the `gistbot` user) are used:
   - `/home/gistbot/.config/gistbot/gistbot.env` for bot configuration.
   - `/home/gistbot/.config/gistbot/opencode-api.env` for the OpenCode API server password.

3. **Install and start systemd units:**
   Set up the systemd user units `opencode-gist.service` and `gistbot.service` following the steps in [docs/install.md](docs/install.md).

4. **Authenticate OpenCode:**
   Ensure OpenCode is authenticated under the `gistbot` system user account so it can contact your configured model provider.

## Configuration

Configuration comes from process environment variables. In production, systemd supplies `/home/gistbot/.config/gistbot/gistbot.env` via `EnvironmentFile`. The binary does **not** auto-load `.env`; for a foreground run, source the environment file first. Use [.env.example](.env.example) as the template. Never commit env files.

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

Model references use `provider/model` or `provider/model#variant`. The bot resolves the `free` alias to the configured free model unless a per-job free override is supplied.

`config.Load` reads only the getter it is given, so tests and the production systemd environment behave the same way. Errors name the offending variable but never print its value.

## Commands

### Telegram commands

- `/gist <url> [focus]` — Queue a repository for analysis with optional focus.
- `/status` — View current running job and queued jobs.
- `/cancel [id]` — Cancel the active job or remove a queued job.
- `/list` — List recent completed jobs (newest first, fits in one message).
- `/show [repo] [doc]` — Resend gist documents from the latest job or a specified repo/doc.
- `/log [id]` — Fetch log tail and `.log` file attachment for a job.
- `/model [alias|provider/model]` — View or switch active model alias (`free`, `flash`, `pro`) or specify a model.
- `/runner [cli|api]` — View or switch the active runner backend.
- `/help` — Show command reference and usage details.

### Build commands

```bash
make build      # build bin/gistbot from ./cmd/gistbot
make test       # go test ./...
make vet        # go vet ./...
make test-race  # go test -race ./...
```

## Security model

- **Owner gate:** Only messages matching `OWNER_USER_ID` are processed; all others are ignored.
- **Fixed argv / no shell:** Subprocesses (`git`, `opencode`, `systemctl`) are invoked directly via `exec.Command` with rigid argument vectors. No shell is invoked, avoiding shell-injection vulnerabilities.
- **GitHub-only clone host:** Repository URLs are validated against `ALLOWED_HOSTS`, accepting only `github.com`.
- **Symlink stripping:** Dangerous symlinks inside cloned repositories are stripped or bypassed to prevent directory traversal.
- **Sandboxed `WORK_DIR`:** Jobs operate inside isolated temporary subdirectories within `WORK_DIR`. Traversal outside this directory is prohibited.
- **Secret redaction:** Bot tokens and passwords are never logged, formatted into user messages, or output in error text. Errors cite variable names, not secret values.
- **Loopback Basic auth:** The OpenCode API server binds to localhost (`127.0.0.1:4096`) protected by HTTP Basic authentication.

See [SECURITY.md](SECURITY.md) for vulnerability reporting guidelines and [AGENTS.md](AGENTS.md) for coding and security invariants.

## Status

- The automated test suite is green (`make test` and `make test-race` pass).
- The manual phone acceptance checklist in [docs/install.md](docs/install.md) has not yet been performed in this checkout.
- Gistbot is pre-1.0 software with no tagged releases yet and no hosted service.

## Project layout

```text
.
├── AGENTS.md                  # security and contribution rules
├── CODE_OF_CONDUCT.md         # code of conduct
├── CONTRIBUTING.md            # contribution guidelines
├── LICENSE                    # MIT license
├── Makefile                   # fixed build and test commands
├── README.md                  # project overview and documentation
├── SECURITY.md                # security policy and reporting
├── cmd/
│   └── gistbot/               # entry point: wiring and graceful shutdown
├── deploy/
│   ├── gistbot.service        # systemd user unit for Gistbot
│   └── opencode-gist.service  # systemd user unit for OpenCode API server
├── docs/
│   └── install.md             # installation and operations guide
├── internal/
│   ├── bot/                   # command dispatch, rendering, document browsing
│   ├── config/                # typed, validated environment configuration
│   ├── jobs/                  # job manager, queue, CLI and API runners
│   ├── opencode/              # loopback OpenCode API client and model listing
│   ├── repo/                  # GitHub URL validation, cloning, gist documents
│   ├── state/                 # persisted model, runner, and job state
│   └── telegram/              # Telegram client and long-poll loop
└── scripts/
    └── host-install.sh        # host installation script
```

## Contributing / License

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting pull requests, and follow our [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

For vulnerability reporting, see [SECURITY.md](SECURITY.md).

Gistbot is open-source software licensed under the [MIT License](LICENSE).
