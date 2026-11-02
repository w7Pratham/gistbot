# Contributing to Gistbot

Thank you for your interest in contributing to Gistbot!

Gistbot is an open-source, owner-only Telegram bot that turns a GitHub repository into three gist documents (`codemap.md`, `routing.md`, and `observation.md`) using OpenCode.

All contributions are accepted under the [MIT License](LICENSE) with no Contributor License Agreement (CLA) required.

---

## Prerequisites

- **Go 1.25 or newer**: Make sure Go is installed and available in your `PATH`.
- **Standard library only**: Gistbot strictly relies on the Go standard library. Do not add external dependencies, third-party packages, or new modules to `go.mod`.
- **Make**: Used for standard build and test targets.
- **Git**: For version control.

---

## Development Workflow

### Fixed Build & Test Commands

We use standard `make` targets to build and test Gistbot. Do not invent alternate build systems or scripts:

```bash
make build       # Compiles bin/gistbot from ./cmd/gistbot
make test        # Runs go test ./...
make vet         # Runs go vet ./...
make test-race   # Runs go test -race ./...
```

All tests, race checks, and vetting must pass before opening a pull request.

### Test-Driven Development (TDD)

We follow strict test-driven development:
1. Write or update a test covering the new feature, bug fix, or edge case first.
2. Run tests to confirm failure for the expected reason (`make test`).
3. Implement the minimal change required to satisfy the test.
4. Run `make test`, `make vet`, and `make test-race` to ensure the entire suite passes cleanly without race conditions.

---

## Non-Negotiable Invariants

Before writing or submitting any code, review the project rules in [AGENTS.md](AGENTS.md):

1. **No arbitrary shell execution**:
   - Subprocesses (`git`, `opencode`, `systemctl`) must be executed directly via `exec.Command` with rigid argv slices.
   - Never pass user, job, model, or environment input to a shell (`sh -c`, `bash -c`, or command string expansion).
2. **Go standard library only**:
   - No third-party modules or external dependencies without an explicit architectural decision.
3. **Secrets never leave the process**:
   - Never log, print, format, serialize, or embed credentials (Telegram bot tokens, OpenCode API passwords, auth headers) in error messages, logs, or user output.
   - Errors cite variable names (e.g., `OPENCODE_API_PASSWORD`), never secret values.
4. **Never commit secrets or deployment artifacts**:
   - Never commit `.env`, `gistbot.env`, `opencode-api.env`, `bin/`, temporary files, or local logs.
5. **Only `github.com` is a valid clone host**:
   - `ALLOWED_HOSTS` only accepts `github.com`. Do not relax or bypass host verification.
6. **Stay within `WORK_DIR`**:
   - Always validate, clean, and confine file system paths. Constructing paths from unvalidated user input or directory traversal is strictly forbidden.
   - Symlinks inside cloned repositories must not point outside their sandboxed tree.
7. **Commit hygiene**:
   - One logical change per commit. Keep commits clean, focused, and well-described.

---

## Proposing Substantial Changes

For major architectural changes, additions to bot command syntax, or modifications to security invariants, please open an issue describing the proposal first before investing substantial time in an implementation. This ensures alignment with the project's scope and owner-only design.

---

## Pull Request Checklist

Before submitting your pull request, ensure that:

- [ ] New behavior or bug fixes are covered by tests written first (TDD).
- [ ] `make test` passes.
- [ ] `make vet` passes without warnings.
- [ ] `make test-race` passes without data races.
- [ ] No third-party Go dependencies were added (`go.mod` stays stdlib-only).
- [ ] No secrets, credentials, tokens, or environment dumps appear in commits, tests, or error messages.
- [ ] Direct `exec.Command` argv lists are used (no shell invocation).
- [ ] Relevant documentation (`README.md`, `docs/`, etc.) is updated.
- [ ] Git commit history is clean and follows standard conventional commit phrasing (e.g., `feat:`, `fix:`, `docs:`, `test:`).

---

## Code of Conduct

All contributors and maintainers are expected to follow our [Code of Conduct](CODE_OF_CONDUCT.md). Please treat everyone with respect and empathy.
