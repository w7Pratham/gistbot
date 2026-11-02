# AGENTS.md — Gistbot

Gistbot is a self-hosted, owner-only Telegram bot. Technical
identifiers remain `gistbot`. Read this checklist before changing code.

## Non-negotiable rules

1. **No arbitrary shell execution.** Never accept a command string from a user,
   job, model, or environment and pass it to a shell. Use `exec.Command` with a
   fixed argv and no `sh -c`. This task and all later runners must keep that
   invariant.
2. **Go 1.25 standard library only.** Do not add third-party modules or new
   dependencies without an explicit design decision.
3. **Secrets never leave the process.** Never log, print, serialize, or put in an
   error message a Telegram bot token, OpenCode API password, or any other
   credential. Error messages name variables, never their values.
4. **Never commit `.env`.** Also never commit the deployed
   `gistbot.env`/`opencode-api.env`, `bin/`, `.m0/`, `.superpowers/`, generated
   logs, state files, or `docs/m0-gistbot.md`. Keep the existing `.gitignore`
   entries.
5. **Configuration uses the injected getter.** `config.Load(getenv)` must use
   only the supplied function. Do not call `os.Getenv` inside the package.
6. **Only `github.com` is a valid clone host.** `ALLOWED_HOSTS` accepts exactly
   `github.com`; do not relax this without a security review.
7. **Stay within `WORK_DIR`.** Never construct job paths from unvalidated user
   input. Resolve, clean, and verify paths; reject traversal.
8. **Fixed commands.** Use the Makefile targets (`make build`, `make test`,
   `make vet`, `make test-race`). Do not invent alternate build systems.
9. **TDD.** Write or update tests first, confirm they fail for the right reason,
   then implement and rerun the focused package and the full suite.
10. **Commit hygiene.** One logical change per commit; do not stage `.m0/`,
    `.superpowers/`, concurrent evidence files, or unrelated changes.

## Configuration contract

- Required: `TELEGRAM_BOT_TOKEN`, `OWNER_USER_ID`, `WORK_DIR`, `OPENCODE_BIN`,
  `OPENCODE_API_PASSWORD`, `MODEL_FREE`, `MODEL_FLASH`, `MODEL_PRO`,
  `STATE_FILE`, `LOG_DIR`.
- Defaults: `TELEGRAM_API_URL=https://api.telegram.org`,
  `OPENCODE_API_URL=http://127.0.0.1:4096`,
  `OPENCODE_API_UNIT=opencode-gist.service`, `DEFAULT_MODEL=flash`,
  `DEFAULT_RUNNER=cli`, `JOB_TIMEOUT=20m`, `CLONE_TIMEOUT=5m`, `MAX_QUEUE=3`,
  `ALLOWED_HOSTS=github.com`.
- `MAX_QUEUE` is constrained to 1–10. `DEFAULT_RUNNER` is `cli` or `api`.
- Model references are `provider/model` or `provider/model#variant`.

## Deployment contract

- `make build` produces `bin/gistbot` from `./cmd/gistbot`.
- Two systemd **user** units: `opencode-gist.service` is an always-on OpenCode
  API server on `127.0.0.1:4096`; `gistbot.service` runs the bot. The bot never
  starts the API server normally and only restarts it once after an unhealthy
  API error.
- `opencode-gist.service` reads `OPENCODE_PASSWORD` from
  `/home/gistbot/.config/gistbot/opencode-api.env`. `gistbot.service` reads
  `OPENCODE_API_PASSWORD` and all bot configuration from
  `/home/gistbot/.config/gistbot/gistbot.env`. The API unit must never receive
  `TELEGRAM_BOT_TOKEN`.
- Both environment files are owned by `gistbot`, mode `0600`, and never
  committed.
- `gistbot.service` sets `XDG_RUNTIME_DIR=/run/user/%U` and
  `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%U/bus` so its
  `systemctl --user restart opencode-gist.service` call can reach the user
  manager.
- Linger is required: `loginctl enable-linger gistbot`. Installation and
  operation are documented in `docs/install.md`.

## Security checklist for reviews

- [ ] No secret appears in a log line, error, test failure, or commit.
- [ ] The API unit receives only `OPENCODE_PASSWORD`; the Telegram token is not
      present in `opencode-api.env`.
- [ ] Deployment environment files are mode `0600`, owned by `gistbot`, and
      untracked.
- [ ] New environment variables are validated and documented in
      `.env.example` and `README.md`.
- [ ] Paths supplied by users or jobs are absolute, cleaned, and confined.
- [ ] Network destinations come from validated configuration; clone hosts are
      exactly `github.com`.
- [ ] No `sh -c`, no shell expansion, no user-controlled argv concatenation.
- [ ] `go vet ./...`, `go test ./...`, and `make test-race` pass.
- [ ] `.gitignore` still covers `.env`, `bin/`, `.m0/`, `.superpowers/`, and
      local test artifacts.
