# spm

Run applications as persistent systemd services without writing unit files.

`spm` gives systemd a small, PM2-like command line. It creates native units, uses journald for logs, and does not run its own daemon.

> spm runs on Linux systems that use systemd. User services do not require root. Use `--system` explicitly when a service must run at the system level.

## Install

Install the latest release for your Linux architecture:

```bash
curl -fsSL https://raw.githubusercontent.com/eyvant/spm/main/install.sh | sh
```

The installer verifies the release checksum and installs `spm` to `~/.local/bin`. Make sure that directory is in `PATH`.

Install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/eyvant/spm/main/install.sh | \
  sh -s -- --version v0.1.0
```

Or install from source with Go 1.21 or later:

```bash
go install github.com/eyvant/spm/cmd/spm@latest
```

Verify the installation:

```bash
spm version
spm doctor
```

## Start your first service

From the directory containing your application:

```bash
spm start ./my-server --name my-server
```

That one command:

1. Creates a native systemd user service.
2. Uses the current directory as its working directory.
3. Starts the process immediately.
4. Enables it to start again with the user manager.
5. Restarts it if it exits.
6. Sends stdout and stderr to journald.

Check it:

```bash
spm list
spm logs my-server -f
spm show my-server
```

Manage it later from any directory:

```bash
spm restart my-server
spm stop my-server
spm start my-server
spm delete my-server
```

For services that must survive logout and start after a reboot, run this once:

```bash
spm startup
```

spm detects whether user lingering is already enabled and only prints a reminder when action is needed.

## Common examples

### Node.js

```bash
spm start app.js --name api
```

Use the explicit command form when the application needs arguments:

```bash
spm start --name api -- node app.js --port 3000
```

### Python, Ruby, shell, and custom interpreters

```bash
spm start worker.py --name worker
spm start server.rb --name web
spm start cleanup.sh --name cleanup
spm start worker.ts --name typescript-worker --interpreter bun
```

spm recognizes `.js`, `.mjs`, `.cjs`, `.py`, `.rb`, and `.sh`. Use `--interpreter` for anything else.

### Native commands and application arguments

Put a complete command after `--`:

```bash
spm start --name proxy -- /opt/proxy/bin/proxy --port 8080 --verbose
```

Everything after `--` belongs to the application, even when an argument has the same name as an spm option:

```bash
spm start --name public-api -- ./server --name internal-api --port 3000
```

### Working directory and environment

```bash
spm start app.js \
  --name api \
  --cwd /opt/api \
  --env NODE_ENV=production \
  --env-file /opt/api/.env \
  --restart always \
  --restart-sec 5s
```

Use `--env-file` for secrets. Values passed with `--env` are stored in the generated unit and saved configuration.

## Which command should I use?

| Goal | Command |
| --- | --- |
| Keep a process running | `spm start` |
| Run a job on a schedule and let it exit | `spm schedule` |
| Restart a long-running service periodically | `spm start --cron-restart` |
| Run a service as root or another system user | `sudo spm --system start` |
| Manage several services from a file | `spm apply spm.toml` |

The distinction between `start` and `schedule` is important: use `start` for servers and workers, and `schedule` for backups, checks, synchronization jobs, and other commands that finish.

## Everyday commands

| Command | Purpose |
| --- | --- |
| `spm list` | List managed services |
| `spm list --watch` | Continuously refresh CPU, memory, and status |
| `spm show NAME` | Show an spm configuration and runtime summary |
| `spm status NAME` | Show native `systemctl status` output |
| `spm logs NAME -f` | Follow logs |
| `spm restart NAME` | Restart using the current unit |
| `spm restart --update NAME` | Regenerate the unit from saved configuration, then restart |
| `spm stop NAME` | Stop now, but keep boot enablement |
| `spm disable NAME` | Stop and disable boot startup |
| `spm enable NAME` | Enable and start |
| `spm delete NAME` | Stop and remove the service and companion units |
| `spm cat NAME` | Print the generated unit and drop-ins |
| `spm edit NAME` | Open a persistent systemd drop-in editor |

Lifecycle commands accept several names or `all`:

```bash
spm restart api worker
spm stop all
spm delete all --yes
```

`all` only selects units created by spm. Deleting all services requires `--yes`.

### Logs

```bash
spm logs api                 # last 100 entries
spm logs api -n 20           # last 20 entries
spm logs api -f              # follow
spm logs api worker -f       # merge several services
spm logs                     # merge all spm services
spm logs api --since 1h --grep ERROR
spm logs api --boot --output json
```

### Change an existing service

Repeat the original start command with `--force`:

```bash
spm start app.js --name api --env NODE_ENV=production --force
```

Settings omitted from a `--force` update are normally inherited. Clear them explicitly when needed:

```bash
spm start app.js --name api --force --clear-env
spm start app.js --name api --force --no-cron-restart
spm start app.js --name api --force --no-max-memory
```

Small changes can also use `set` and `unset`:

```bash
spm set api --env NODE_ENV=production --restart-sec 5s
spm unset api --env OLD_KEY
```

spm keeps previous saved configurations:

```bash
spm history api
spm rollback api
spm rollback api --revision 2
spm clone api api-test
```

## Scheduled jobs

Create a oneshot job that runs every 30 minutes:

```bash
spm schedule --name checker \
  --cron "*/30 * * * *" \
  --persistent \
  -- node ./scripts/check.js
```

Run it once immediately without waiting for the timer:

```bash
spm run checker
```

Or run immediately while creating the schedule:

```bash
spm schedule --name checker --cron "*/30 * * * *" \
  --run-now -- node ./scripts/check.js
```

Interval timers are also supported:

```bash
spm schedule --name cache-sync \
  --on-boot 5min \
  --interval 30min \
  --randomized-delay 2min \
  -- ./sync-cache
```

`--persistent` catches up a missed calendar run after downtime. `--randomized-delay` helps avoid many hosts starting the same job together. Use `--timezone Asia/Singapore` with `--cron` when the server's local timezone is not appropriate.

### Periodically restart a long-running service

This is different from a scheduled job:

```bash
spm start server.js --name api \
  --cron-restart "0 4 * * *" \
  --timezone Asia/Singapore
```

spm creates and manages a companion systemd timer. Cron expressions use five fields. Generated systemd calendar expressions are validated before installation.

## System services

User mode is the default and does not need sudo. Use system mode only when the process needs system-level privileges, ownership, or dependencies:

```bash
sudo spm --system start /opt/api/server --name api
sudo spm --system status api
sudo spm --system logs api -f
sudo spm --system restart api
```

Run a system service as a non-root account:

```bash
sudo spm --system start /opt/api/server \
  --name api \
  --user www-data \
  --group www-data
```

The executable, working directory, environment file, and writable paths must be accessible to that account. spm resolves relative commands using the target user's home directory and search paths.

Important rules:

- Put global `--system` before the command.
- Keep using `--system` for every later operation on that service.
- User and system services are separate namespaces and may have the same name.
- Do not run `sudo spm` without `--system`; that targets root's user manager instead of the system manager.

User units live in `~/.config/systemd/user`. System units live in `/etc/systemd/system`.

## Declarative configuration

Use `spm.toml` when several services should be reviewed and applied together:

```toml
[[services]]
name = "api"
cwd = "/opt/api"
command = ["node", "app.js", "--port", "3000"]
restart = "always"
restart_sec = "3s"
env_file = "/opt/api/.env"
max_memory = "512M"

[[services]]
name = "worker"
cwd = "/opt/worker"
script = "worker.py"
args = ["--queue", "default"]
interpreter = "python3"
enable = false

[[services]]
name = "nightly-backup"
cwd = "/opt/backup"
command = ["./backup"]
schedule = "0 2 * * *"
persistent = true
```

Preview and apply:

```bash
spm validate spm.toml
spm diff spm.toml
spm apply --dry-run spm.toml
spm apply spm.toml
```

Export existing services:

```bash
spm export all > spm.toml
```

Apply is declarative for services present in the file: omitted optional settings return to defaults. Services not listed in the file are not deleted. The file's directory is the default working directory.

## Readiness and health checks

Wait for an application to become ready during `spm start`:

```bash
spm start --name api \
  --wait-http http://127.0.0.1:3000/health \
  --wait-timeout 30s \
  -- node server.js
```

HTTP, TCP, and command readiness probes are available through `--wait-http`, `--wait-tcp`, and `--wait-command`. They run in the invoking spm process and do not create background services.

Add a periodic health check:

```bash
spm start --name api \
  --health-http http://127.0.0.1:3000/health \
  --health-interval 30s \
  --health-timeout 5s \
  --health-retries 3 \
  --health-action restart \
  -- node server.js
```

Inspect health state and logs:

```bash
spm health api
spm logs api --health
```

Periodic checks use short-lived systemd oneshot units and timers; spm still does not run a daemon. The default action is `none`, which only records failures. Available actions are `none`, `restart`, and `stop`.

## Resource limits and security

Resource options map directly to systemd cgroup controls:

```bash
spm start --name api \
  --max-memory 512M \
  --max-cpu 200% \
  --max-tasks 100 \
  --open-files 65535 \
  -- node server.js
```

`--max-memory` is a hard cgroup limit. If the process is OOM-killed, its restart policy determines whether it starts again.

Apply a security preset:

```bash
spm start --name api --sandbox basic -- node server.js
spm security api
```

`basic` protects the home directory and system paths. `strict` adds kernel, module, and SUID restrictions. The working directory remains writable. User-mode sandboxing requires unprivileged user namespaces; run `spm doctor` to check support.

Advanced systemd settings are available without editing a unit:

```bash
spm start --name api \
  --after network-online.target \
  --requires redis.service \
  --exec-start-pre '/usr/bin/test -f /opt/api/config.json' \
  --exec-reload '/bin/kill -HUP $MAINPID' \
  --property Nice=5 \
  -- node server.js

spm reload api
```

Core properties controlled by spm, such as `ExecStart`, `WorkingDirectory`, `Environment`, `Restart`, and `User`, cannot be overridden with `--property`.

## Multiple instances

Run several fork-mode instances with a systemd template unit:

```bash
spm start server.js --name api --instances 4 \
  --env 'PORT=300{{instance}}'

spm scale api 8
spm restart api
spm logs api -f
```

Each instance receives `SPM_INSTANCE` and `NODE_APP_INSTANCE`. `{{instance}}` is replaced in arguments, environment values, and PID files. Restarts are rolling and scaling leaves unchanged instances running.

This is not PM2 cluster mode: instances do not share a listening socket. Give each instance a different port behind a load balancer, or make the application use `SO_REUSEPORT`. Instances cannot currently be combined with schedules, cron restart, readiness probes, or health checks.

## Optional: mise integration

If a project uses mise, add `--mise` to run the application through `mise exec --`:

```bash
cd /opt/my-app
mise install
spm start --name api --mise -- node app.js --port 3000
```

The shorter script form works too:

```bash
spm start app.js --name api --mise
```

spm does not pin the resolved Node.js or other tool binary. Tool versions and `[env]` from `mise.toml` are reevaluated whenever the service starts. After changing a version, use a normal restart:

```bash
mise use node@24
spm restart api
```

Use `--no-mise` with `--force` to remove an inherited mise setting. In system mode with `--user`, spm finds mise from the target user's home directory and search paths instead of root's environment.

Snap normally launches mise through `snap run`, which can move child processes out of the service cgroup. spm detects a classic Snap installation and invokes its internal mise executable directly, preserving process cleanup, resource accounting, limits, and unit-scoped logs.

mise support is optional. spm does not install mise or any language runtime.

## Native systemd access

spm creates ordinary systemd units, so native tools continue to work:

```bash
systemctl --user status spm-api.service
journalctl --user --unit spm-api.service --follow
systemctl --user cat spm-api.service
```

`spm status` intentionally shows native systemctl output. `spm show` and `spm describe` show the friendlier spm summary. `spm cat` prints the final unit, while `spm edit` stores manual changes in a drop-in that survives `spm start --force`.

Existing simple units can be imported:

```bash
spm import /etc/systemd/system/api.service --name api
```

spm refuses unit semantics it cannot preserve safely, such as socket activation and oneshot services.

Saved normalized configurations use mode `0600` and live in:

```text
~/.config/spm/services/<name>.json
/etc/spm/services/<name>.json    # system mode
```

## PM2 command map

| PM2 | spm |
| --- | --- |
| `pm2 start index.js --name api` | `spm start index.js --name api` |
| `pm2 ls` | `spm list` |
| `pm2 show api` | `spm show api` |
| `pm2 logs api` | `spm logs api -f` |
| `pm2 restart api` | `spm restart api` |
| `pm2 reload api` | `spm reload api` |
| `pm2 stop api` | `spm stop api` |
| `pm2 delete api` | `spm delete api` |
| `pm2 sendSignal SIGUSR2 api` | `spm signal SIGUSR2 api` |
| `pm2 jlist` | `spm list --json` |
| `pm2 monit` | `spm list --watch` |
| `pm2 start app.js -i 4` in fork mode | `spm start app.js -i 4` |
| `pm2 scale api 8` | `spm scale api 8` |
| `pm2 startup` | `spm startup` |
| `pm2 save` | Not needed; spm enables services when they are created. |
| PM2 ecosystem file | `spm export all > spm.toml`, then `spm apply spm.toml` |

## Shell completion

Bash:

```bash
mkdir -p ~/.local/share/bash-completion/completions
spm completion bash > ~/.local/share/bash-completion/completions/spm
```

Zsh:

```zsh
mkdir -p ~/.zfunc
spm completion zsh > ~/.zfunc/_spm
fpath=(~/.zfunc $fpath)
autoload -Uz compinit && compinit
```

Fish:

```fish
mkdir -p ~/.config/fish/completions
spm completion fish > ~/.config/fish/completions/spm.fish
```

Completion understands subcommands, options, enum values, and managed service names. It switches to system services when `--system` is present.

## Troubleshooting

Start with:

```bash
spm doctor
spm status NAME
spm logs NAME -n 100
```

For system mode:

```bash
sudo spm --system doctor
sudo spm --system status NAME
```

`doctor` checks systemd features, manager access, `XDG_RUNTIME_DIR`, lingering, journald, unit permissions, mise, user namespaces, and whether the spm executable path is stable. `FAIL` returns a non-zero exit status; `WARN` reports a risk without failing.

Every command also has focused help:

```bash
spm --help
spm start --help
spm schedule --help
spm logs --help
```

## Build and test

```bash
go test -race ./...
go vet ./...
go build -o spm ./cmd/spm
```

Release builds inject the version with ldflags:

```bash
go build \
  -ldflags "-X github.com/eyvant/spm/internal/app.version=0.1.0" \
  -o spm ./cmd/spm
```

GitHub Actions runs tests, vet, builds, and real systemd integration tests on Ubuntu 22.04, 24.04, and 26.04. A compatibility matrix covers Debian, Fedora, openSUSE, and Arch Linux and verifies generated units with `systemd-analyze verify`.

## License

spm is available under the [MIT License](LICENSE).

## Current limitations

- Linux with systemd is required.
- PM2 cluster-mode socket sharing, watch mode, deployment, and remote management are not implemented.
- `--instances` provides fork-mode instances, not PM2 cluster mode.
- spm does not install language runtimes or runtime managers.
