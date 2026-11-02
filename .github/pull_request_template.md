## Summary

<!-- Briefly describe the changes in this pull request and the motivation behind them. -->

## Changes Made

<!-- List key changes or touched components. -->

## Testing

<!-- Detail the tests you ran to verify this change. -->
- [ ] Added / updated tests first following TDD
- [ ] `make test` passed
- [ ] `make vet` passed
- [ ] `make test-race` passed

## Security & Architectural Checklist

<!-- Ensure your changes adhere to Gistbot's non-negotiable invariants. -->
- [ ] **No secrets**: No tokens, API keys, passwords, or credentials appear in commits, logs, or error messages.
- [ ] **No arbitrary shell**: Direct `exec.Command` with rigid argument vectors used; no shell invocation (`sh -c`).
- [ ] **Standard library only**: No third-party Go dependencies added to `go.mod`.
- [ ] **Path safety**: All paths remain confined within `WORK_DIR`; no directory traversal or unvalidated paths.
- [ ] **Clone host**: Clones remain restricted strictly to `github.com`.
- [ ] **Documentation**: Updated relevant documentation (`README.md`, `docs/`, etc.) if applicable.
