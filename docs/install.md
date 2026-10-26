# Install and operate Zero Two (GistBot)

Zero Two runs as the dedicated `gistbot` Unix user in two systemd **user**
units:

| Unit | Role |
| --- | --- |
| `opencode-gist.service` | Always-on OpenCode API server on `127.0.0.1:4096` |
| `gistbot.service` | Telegram bot; may restart the API unit after an unhealthy API error |

The bot never starts the API server during normal operation. It only asks
`systemctl --user` to restart `opencode-gist.service` once when an API health
check fails. Both units must be running for the `api` runner.

All paths, users, and service names below are fixed by the project. The bot
binary is `gistbot`; the display name is Zero Two (02).

## 1. M0 setup

Run the setup script as root. It creates the `gistbot` user and home, installs
the OpenCode binary, the `repo-scout` agent, the `repo-gist` skill, and both
environment files:

```bash
sudo bash scripts/m0-setup.sh
```

Fill in the Telegram credentials in:

```text
/home/gistbot/.config/gistbot/gistbot.env
```

- `TELEGRAM_BOT_TOKEN` — the bot token from BotFather.
- `OWNER_USER_ID` — the only Telegram user ID allowed to use the bot.

The same file contains `OPENCODE_API_PASSWORD`. The API server reads the
matching `OPENCODE_PASSWORD` from:

```text
/home/gistbot/.config/gistbot/opencode-api.env
```

M0 generates both values from one random password. Both files are owned by
`gistbot` and must be mode `0600`:

```bash
sudo chown gistbot:gistbot \
  /home/gistbot/.config/gistbot/gistbot.env \
  /home/gistbot/.config/gistbot/opencode-api.env
sudo chmod 0600 \
  /home/gistbot/.config/gistbot/gistbot.env \
  /home/gistbot/.config/gistbot/opencode-api.env
```

Never commit either environment file, print its contents, or copy it into the
repository.

### OpenCode authentication

The bot uses `gistbot`'s own OpenCode authentication, not the operator's. The
M0 setup installs the OpenCode binary under `/home/gistbot/.opencode/bin` and
both units set `HOME=/home/gistbot`. Authenticate as `gistbot` after M0:

```bash
sudo -u gistbot -H bash -lc \
  'cd /home/gistbot && /home/gistbot/.opencode/bin/opencode auth login deepseek'
```

Confirm the installed models:

```bash
sudo -u gistbot -H bash -lc \
  'cd /home/gistbot && /home/gistbot/.opencode/bin/opencode models'
```

Do not run this as the operator account; a login in another home directory is
not visible to the services.

## 2. Build and install the bot

From the repository root:

```bash
make build
```

This produces `bin/gistbot` (ignored by Git). Install the binary and units for
the `gistbot` user:

```bash
sudo install -d -o gistbot -g gistbot /home/gistbot/bin
sudo install -o gistbot -g gistbot -m 0755 bin/gistbot /home/gistbot/bin/gistbot
sudo install -d -o gistbot -g gistbot /home/gistbot/.config/systemd/user
sudo install -o gistbot -g gistbot -m 0644 deploy/*.service \
  /home/gistbot/.config/systemd/user/
```

The unit files reference:

- `/home/gistbot/bin/gistbot`
- `/home/gistbot/.opencode/bin/opencode`
- `/home/gistbot/gists` as `WORK_DIR`
- `/home/gistbot/.config/gistbot/gistbot.env`
- `/home/gistbot/.config/gistbot/opencode-api.env`

The API unit reads only `opencode-api.env`; the Telegram token is never passed
to it.

## 3. Enable linger and start both units

Linger makes the `gistbot` user manager start at boot, without an interactive
login:

```bash
sudo loginctl enable-linger gistbot
```

Reload the user manager and enable both units:

```bash
sudo -u gistbot -H bash -lc 'export XDG_RUNTIME_DIR=/run/user/$(id -u); \
  systemctl --user daemon-reload; \
  systemctl --user enable --now opencode-gist.service gistbot.service'
```

Check the result:

```bash
sudo -u gistbot -H bash -lc 'export XDG_RUNTIME_DIR=/run/user/$(id -u); \
  systemctl --user status opencode-gist.service gistbot.service --no-pager'
```

`XDG_RUNTIME_DIR` is required when invoking `systemctl --user` through `sudo`;
otherwise systemd cannot find the user's bus. The installed `gistbot.service`
also sets `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` so the bot's own
`systemctl --user restart opencode-gist.service` works from inside the service.

After the binary and units are installed, verify the installed unit files as
`gistbot`:

```bash
sudo -u gistbot -H bash -lc 'export XDG_RUNTIME_DIR=/run/user/$(id -u); \
  systemd-analyze --user verify \
    /home/gistbot/.config/systemd/user/gistbot.service \
    /home/gistbot/.config/systemd/user/opencode-gist.service'
```

Running `systemd-analyze --user verify deploy/*.service` from a checkout before
installation fails its executable check: the `gistbot` home paths do not exist
yet or are not readable by the operator account. The check above runs against
the installed files with the `gistbot` account that owns them.

## 4. Logs

Follow both units as `gistbot`, with the user runtime directory set:

```bash
sudo -u gistbot -H bash -lc 'export XDG_RUNTIME_DIR=/run/user/$(id -u); \
  journalctl --user -u gistbot -u opencode-gist -f'
```

Without `-f`, add `--no-pager` for a snapshot. Bot job logs are separate files
under `/home/gistbot/.local/state/gistbot/logs`; use `/log` from Telegram to
read the latest one.

Never enable shell tracing or print environment files while debugging: they
contain the Telegram token and the OpenCode API password.

## 5. Change configuration and restart

Edit the relevant environment file as `gistbot` (keep mode `0600`), then
restart the affected units:

```bash
sudo -u gistbot -H bash -lc 'export XDG_RUNTIME_DIR=/run/user/$(id -u); \
  systemctl --user restart opencode-gist.service gistbot.service'
```

- `opencode-api.env` (`OPENCODE_PASSWORD`) is read by `opencode-gist.service`.
  Restart that unit after changing it.
- `gistbot.env` is read by `gistbot.service` at startup. Restart
  `gistbot.service` after any change, including `TELEGRAM_BOT_TOKEN`,
  `OWNER_USER_ID`, `OPENCODE_API_PASSWORD`, models, paths, or timeouts.
- If the shared API password changes, update both files and restart both
  units. A mismatch makes every API health check fail.
- After changing a unit file itself, run `systemctl --user daemon-reload`
  before restarting.

Stopping the API unit does not stop the bot; the bot only restarts the API unit
when the `api` runner sees an unhealthy API.

## 6. Manual acceptance checklist

These checks require a phone or Telegram client and the live bot. They have
**not** been performed in this repository checkout; record the result, date,
and any failure output next to each item after running them. Do not paste
tokens, passwords, or environment-file contents into the checklist.

### Access and help

- [ ] Open the bot chat: the Telegram command menu lists `/gist`, `/status`,
      `/cancel`, `/list`, `/show`, `/log`, `/model`, `/runner`, and `/help`
      without typing them first.
- [ ] Send `/help` from the owner account: the reply is one compact command
      list with one footer showing the current model, runner, and job state.
- [ ] Send any command from a non-owner account: no reply is delivered.

### UI and job cards

- [ ] Send `/gist https://github.com/<public-small-repo>`: exactly one job
      card appears. It is edited in place to `⚡ Running` when the job starts
      and edited once more to `✅ Gist ready`. No second "Gist ready" message
      appears.
- [ ] A successful job delivers exactly one media group containing three
      documents (`codemap.md`, `routing.md`, `observation.md`); no separate
      document messages arrive.
- [ ] Send two `/gist` commands back to back: the second job's initial card says
      `🕓 Queued #1`, then that same card is edited to `⚡ Running` when the
      first job finishes. Each job keeps one card.
- [ ] Force a failed run (for example, `/gist` a nonexistent GitHub repository,
      or stop the API unit with runner `api` and leave it down): exactly one
      failure card appears with a short newest-output tail in a `<pre>` block,
      followed by exactly one `<job>.log` document. No album is sent.
- [ ] Send `/cancel` during a long job: the same card is edited to
      `🚫 Cancelled`, includes a short log tail, and attaches the `.log` file.
      Then run `pgrep -u gistbot -a 'opencode|git'` and confirm it prints
      nothing.
- [ ] Send `/status` while a job runs: it shows only the running job and the
      queue, with the repository, model, runner, and elapsed time.

### CLI runner

- [ ] With runner `cli`, send `/gist https://github.com/<public-small-repo>`:
      the three documents arrive through the CLI runner as the one album.
- [ ] `/list` lists the installed repository after completion.

### API runner

- [ ] Send `/runner api`, then `/gist https://github.com/<public-small-repo>`:
      the documents arrive through the API runner.
- [ ] Start an API job and send `/cancel` mid-run: the job stops and the
      notification reports `cancelled`.

### Browsing and safety

- [ ] `/show` with no arguments sends the newest installed repository's
      documents as one album; `/show <repo>` does the same for an explicit
      repository.
- [ ] `/show <repo> codemap` returns only `codemap.md` as a single document.
- [ ] `/show ../../etc/passwd` is rejected and no file outside `WORK_DIR` is
      read.
- [ ] `/log` returns a compact tail card under 4,096 characters and attaches
      the full `.log` file.
- [ ] `/list` shows installed repositories newest first and stays within one
      Telegram message.

### Models

- [ ] `/model free <installed-provider/model>` persists the selection and the
      next `/gist` uses it.
- [ ] If the OpenCode free tier is exhausted, the bot reports a clear error
      and the default `flash` model still works.
- [ ] `/model free reset` restores `MODEL_FREE`.

### Restart, refresh, and API failure

- [ ] Reboot the PC, wait for boot, then send `/help` without logging in to the
      `gistbot` account. The bot answers because linger is enabled.
- [ ] Re-gist the same repository: fresh documents replace the old ones.
- [ ] Force a failed run for an installed repository: the old repository and
      its documents remain available.
- [ ] With runner `api`, stop the API unit manually:
      `sudo -u gistbot -H bash -lc 'export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user stop opencode-gist.service'`,
      then send `/gist …`. The bot restarts `opencode-gist.service` once and
      the job runs. If the API stays down, the bot reports a failure and does
      not silently switch to the CLI runner.

## 7. Security notes

- `gistbot.env` and `opencode-api.env` are mode `0600`, owned by `gistbot`,
  and never committed.
- `opencode-gist.service` receives only `OPENCODE_PASSWORD`; it never receives
  `TELEGRAM_BOT_TOKEN`.
- The API server binds to `127.0.0.1:4096` and requires HTTP Basic
  authentication. Do not expose it on a public interface.
- The bot uses fixed argv for external commands and never invokes a shell.
- `WORK_DIR` jobs are confined to `/home/gistbot/gists`; only `github.com`
  clone URLs are accepted.
