# AGENTS.md — Zero Two (gistbot)

Zero Two (short **02**) is a private, owner-only Telegram bot. Technical
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
4. **Never commit `.env`.** Also never commit `bin/`, `.m0/`, `.superpowers/`,
   generated logs, state files, or `docs/m0-gistbot.md`. Keep the existing
   `.gitignore` entries.
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

## Security checklist for reviews

- [ ] No secret appears in a log line, error, test failure, or commit.
- [ ] New environment variables are validated and documented in
      `.env.example` and `README.md`.
- [ ] Paths supplied by users or jobs are absolute, cleaned, and confined.
- [ ] Network destinations come from validated configuration; clone hosts are
      exactly `github.com`.
- [ ] No `sh -c`, no shell expansion, no user-controlled argv concatenation.
- [ ] `go vet ./...`, `go test ./...`, and `make test-race` pass.
- [ ] `.gitignore` still covers `.env`, `bin/`, `.m0/`, `.superpowers/`, and
      local test artifacts.
