# Security Policy

The security of Gistbot is taken seriously. Gistbot handles code cloning, agent execution, and Telegram communication, so maintaining strong security boundaries is essential.

---

## Supported Versions

Gistbot is currently pre-1.0 software. Only the latest commit on the `main` branch receives security updates. Previous releases or commits are not backported.

| Version | Supported |
| --- | --- |
| `main` | :white_check_mark: |
| `< 1.0` | :x: |

---

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

If you believe you have discovered a vulnerability, please report it privately through **GitHub Security Advisories**:

👉 **[Report a vulnerability via GitHub Security Advisories](https://github.com/w7Pratham/gistbot/security/advisories/new)**

This private reporting channel allows the maintainer to review, triage, and patch the issue before public disclosure.

### What to Include in Your Report

To help us understand and resolve the issue quickly, please provide:

- A clear description of the vulnerability and its potential impact.
- Step-by-step reproduction instructions or a minimal proof of concept (PoC).
- The affected component, function, or runner backend (`cli` or `api`).
- Any suggestions or mitigations you might have.

---

## Scope

### In-Scope Vulnerabilities

We welcome reports regarding security vulnerabilities within Gistbot's codebase and operating model, including:

- **Secret leakage**: Exposure of Telegram bot tokens, OpenCode API passwords, or authorization credentials via logs, error outputs, or Telegram messages.
- **Sandbox escape / path traversal**: Arbitrary file reads, writes, or execution escaping the designated `WORK_DIR` sandbox.
- **Prompt injection reaching host files**: Prompt injection or malicious repository inputs executing unintended commands or accessing the host system.
- **Clone-host validation bypass**: Bypassing `ALLOWED_HOSTS` or cloning from unauthorized hosts other than `github.com`.
- **Owner-gate bypass**: Circumventing `OWNER_USER_ID` checks to execute commands or interact with the bot from unauthorized Telegram accounts.
- **Authorization header leakage**: Leakage of loopback API Basic authentication headers or credentials.

### Out of Scope

The following areas are considered out of scope:

- Subjective model response quality or accuracy from the underlying OpenCode model provider.
- Upstream bugs or vulnerabilities in OpenCode itself or Telegram's API infrastructure (please report those upstream).
- Vulnerabilities resulting from operator misconfiguration (e.g., exposing the loopback API publicly, running as `root`, world-readable environment files, or relaxing systemd permissions).
- Denial of Service (DoS) attacks requiring prior root or host-level access to the deployment server.

---

## Response & Disclosure Process

- **Acknowledgement**: The maintainer aims to acknowledge received vulnerability reports on a best-effort basis within **7 business days**.
- **Triage & Remediation**: If verified, a fix will be developed and tested privately before publication.
- **Bounty**: Gistbot is an unpaid, open-source hobby project; there is **no bug bounty program** or monetary reward.

Thank you for helping keep Gistbot secure!
