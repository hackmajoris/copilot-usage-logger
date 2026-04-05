# copilot-usage-logger

A standalone HTTPS MITM proxy that intercepts traffic to `api.githubcopilot.com`
and logs token usage, model calls, and premium request weights — per task and globally.

No mitmproxy or Python required. Written in Go using only the standard library.

---

## How it works

1. Listens as an HTTP/HTTPS proxy on a local port.
2. On first run, generates a self-signed CA (`ca.crt` / `ca.key`).
3. When a CONNECT tunnel is opened to `api.githubcopilot.com`, it performs a MITM:
   signs a leaf certificate on the fly and decrypts the TLS stream.
4. POST responses are parsed for SSE `data:` lines containing `usage` fields.
5. Token counts, model names, and premium weights are written to log files and stdout.

---

## Quick start

Follow these steps in order. Each step must be completed before the next.

### Step 1 — Choose a working directory

The proxy writes four files on first run (`ca.crt`, `ca.key`, `copilot_usage.log`,
`copilot_data.json`). Pick a permanent location you control and create it if needed:

```bash
mkdir -p ~/copilot-logger
cd ~/copilot-logger
```

All subsequent commands assume you are inside this directory.

---

### Step 2 — Install the binary

**Option A: `go install` (requires Go 1.21+)**

```bash
go install github.com/hackmajoris/copilot-usage-logger@latest
```

The binary lands in `$(go env GOPATH)/bin/copilot-logger`. Make sure that directory
is on your `PATH`:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

**Option B: Download a pre-built binary**

Go to the [Releases page](https://github.com/hackmajoris/copilot-usage-logger/releases/latest)
and download the archive for your OS and architecture:

| OS      | Architecture             | File                                       |
|---------|--------------------------|--------------------------------------------|
| macOS   | Apple Silicon (M1/M2/M3) | `copilot-usage-logger_darwin_arm64.tar.gz` |
| macOS   | Intel                    | `copilot-usage-logger_darwin_amd64.tar.gz` |
| Linux   | x86-64                   | `copilot-usage-logger_linux_amd64.tar.gz`  |
| Linux   | ARM64                    | `copilot-usage-logger_linux_arm64.tar.gz`  |
| Windows | x86-64                   | `copilot-usage-logger_windows_amd64.zip`   |
| Windows | ARM64                    | `copilot-usage-logger_windows_arm64.zip`   |

Extract into your working directory:

```bash
# macOS / Linux
tar -xzf copilot-usage-logger_darwin_arm64.tar.gz -C ~/copilot-logger

# Windows (PowerShell)
Expand-Archive copilot-usage-logger_windows_amd64.zip -DestinationPath $HOME\copilot-logger
```

Verify the download with `checksums.txt` (included in the release):

```bash
sha256sum --check checksums.txt
```

**Option C: Build from source (requires Go 1.21+)**

```bash
git clone https://github.com/hackmajoris/copilot-usage-logger.git ~/copilot-logger
cd ~/copilot-logger
go build -o copilot-logger copilot-logger.go
```

---

### Step 3 — Generate and trust the CA certificate

Run the `--trust-cert` command. It generates `ca.crt` / `ca.key` if they don't exist,
then installs the certificate as a trusted root CA in your OS keychain.

```bash
copilot-logger --trust-cert
```

You will be prompted for your password (macOS / Linux) or need to run as Administrator
(Windows). The command handles all platforms automatically:

| OS      | Method                                              |
|---------|-----------------------------------------------------|
| macOS   | `sudo security add-trusted-cert` (password prompt)  |
| Linux   | `sudo update-ca-certificates`                       |
| Windows | `certutil -addstore` (run terminal as Administrator)|

If you regenerate the certificate (e.g. after deleting `ca.crt`), re-run
`--trust-cert` — it removes the old entry before installing the new one.

> **Keep `ca.key` private.** Anyone with this file can sign certificates trusted by
> your machine. Never commit it to version control.

---

### Step 5 — Start the proxy

Open a dedicated terminal window (or a tmux/screen session) in your working directory
and start the proxy. Leave this terminal running for as long as you want to capture
usage.

```bash
cd ~/copilot-logger
copilot-logger
```

To label the session so you can group stats later:

```bash
copilot-logger -task my-feature
```

The proxy is now listening on `http://127.0.0.1:8080`.

---

### Step 6 — Set proxy variables in your working terminal

Open a **second terminal** (the one you will use to run your editor, CLI tools, or
agents) and run:

**macOS / Linux**
```bash
eval "$(copilot-logger --print-proxy)"
```

**PowerShell**
```powershell
copilot-logger --print-proxy | Invoke-Expression
```

**CMD**
```cmd
for /f "tokens=*" %i in ('copilot-logger --print-proxy') do %i
```

This sets `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, and `https_proxy` in the
current session. If you started the proxy on a custom port pass the same flag:

```bash
eval "$(copilot-logger -addr :9090 --print-proxy)"
```

> The variables only apply to the current shell session — re-run the command each
> time you open a new terminal.

---

### Step 7 — Open GitHub Copilot or OpenCode

With the proxy running and the environment variables set, start your AI tool in the
same terminal where you exported the proxy settings:

**VS Code with GitHub Copilot**

Add to VS Code `settings.json` (Cmd+Shift+P → "Open User Settings (JSON)"):

```json
{
  "http.proxy": "http://127.0.0.1:8080",
  "http.proxyStrictSSL": true,
  "github.copilot.advanced": {
    "debug.useNodeFetcher": true,
    "debug.chatOverrideProxyUrl": "http://127.0.0.1:8080"
  }
}
```

Then launch VS Code from the terminal where you exported the proxy variables:

```bash
code .
```

**OpenCode (CLI)**

OpenCode picks up `HTTP_PROXY` / `HTTPS_PROXY` automatically. Just launch it from
the same terminal:

```bash
opencode
```

**Any other CLI tool**

Any tool that respects the standard `HTTP_PROXY` / `HTTPS_PROXY` environment variables
will be captured automatically once those are set.

---

You should now see log lines appearing in the proxy terminal as Copilot requests are
intercepted. Check `copilot_usage.log` or run `copilot-logger --summary` to view
aggregated stats.

---

## Commands

| Command                          | Description                                                        |
|----------------------------------|--------------------------------------------------------------------|
| `copilot-logger`                 | Start the MITM proxy (default mode)                                |
| `copilot-logger --trust-cert`    | Generate CA cert (if needed) and install it as a trusted root CA            |
| `copilot-logger --print-proxy`   | Print shell commands to set `HTTP_PROXY`/`HTTPS_PROXY` (use with `eval`)    |
| `copilot-logger --summary`       | Print current-month usage summary and exit                         |
| `copilot-logger --prevmonth`     | Print previous-month usage summary and exit                        |
| `copilot-logger --version`       | Print the application version and exit                             |
| `copilot-logger --help`          | Print help and exit                                                |

Positional subcommands are also accepted for `summary`, `prevmonth`, and `version`
(e.g. `copilot-logger summary`).

---

## All flags

```bash
copilot-logger \
  -addr         :8080              \
  -task         my-feature         \
  -log          copilot_usage.log  \
  -summary-file copilot_summary.log \
  -data         copilot_data.json  \
  -cacert       ca.crt             \
  -cakey        ca.key
```

| Flag            | Default               | Description                                                                   |
|-----------------|-----------------------|-------------------------------------------------------------------------------|
| `-addr`         | `:8080`               | TCP address the MITM proxy listens on (e.g. `:8080` or `127.0.0.1:9090`)      |
| `-task`         | `default`             | Label used to group token-usage stats in the summary log                      |
| `-log`          | `copilot_usage.log`   | Path to the append-only NDJSON file that records every intercepted request    |
| `-summary-file` | `copilot_summary.log` | Path to the summary file rewritten on each request                            |
| `-data`         | `copilot_data.json`   | Path to the persistent JSON store that accumulates stats across all runs      |
| `-cacert`       | `ca.crt`              | Path to the self-signed CA certificate (created automatically on first run)   |
| `-cakey`        | `ca.key`              | Path to the CA private key (created automatically on first run — keep secret) |
| `--trust-cert`  | —                     | Generate CA cert (if needed) and install it as a trusted root CA              |
| `--print-proxy` | —                     | Print shell commands to set `HTTP_PROXY`/`HTTPS_PROXY` and exit               |
| `--summary`     | —                     | Print current-month usage summary and exit                                    |
| `--prevmonth`   | —                     | Print previous-month usage summary and exit                                   |
| `--version`     | —                     | Print the application version and exit                                        |

---

## Docker container usage

If you run an agent or tool inside a Docker container and want it to go through the
proxy, you need to:

1. Copy `ca.crt` (generated on first run) into your Docker build context.
2. Trust it in the image.
3. Point the container at the host proxy using `host.docker.internal` (Docker Desktop
   on Mac/Windows) or `172.17.0.1` (native Linux Docker).

### Dockerfile snippet

```dockerfile
USER root
COPY ca.crt /usr/local/share/ca-certificates/copilot-logger.crt
RUN apk add --no-cache ca-certificates && update-ca-certificates
```

The cert **must** have a `.crt` extension for `update-ca-certificates` to pick it up.

### Running the container

```bash
docker run \
  -e SSL_CERT_FILE=/usr/local/share/ca-certificates/copilot-logger.crt \
  -e HTTP_PROXY=http://host.docker.internal:8080 \
  -e HTTPS_PROXY=http://host.docker.internal:8080 \
  -e NO_PROXY=localhost,127.0.0.1 \
  your-image
```

### host.docker.internal vs 172.17.0.1

| Environment                    | Host address to use                   |
|--------------------------------|---------------------------------------|
| Docker Desktop (Mac / Windows) | `host.docker.internal`                |
| Native Linux Docker            | `172.17.0.1` (default bridge gateway) |

On Docker Desktop for Mac, containers run inside a Linux VM and cannot reach the host
at `172.17.0.1`. Use `host.docker.internal` instead — it always resolves to the
correct host IP.

Verify the proxy is reachable before troubleshooting TLS:

```bash
docker run --rm alpine sh -c "apk add --no-cache netcat-openbsd && nc -zv host.docker.internal 8080"
```

---

## Output files

### `copilot_usage.log`

Append-only log of every intercepted POST request and its parsed response.

```
[2026-04-05 14:01:02] [my-feature] ► POST https://api.githubcopilot.com/chat/completions

[2026-04-05 14:01:03] [my-feature] ◄ RESPONSE
  Model           : gpt-4o
  Total tokens    : 1842
  Cached tokens   : 512
  Reasoning tokens: 0
  Premium weight  : 0x
```

### `copilot_summary.log`

Overwritten after every response with the latest aggregated stats. The MTD
(month-to-date) section appears first, followed by all-time totals.

```
============================================================
COPILOT USAGE SUMMARY  —  current month: 2026-04
============================================================
  Current month (2026-04):
  ────────────────────────────────────────────────────────────
  Total API calls     : 3
  Total tokens        : 5210
  Cached tokens       : 1024
  Reasoning tokens    : 400
  Premium requests    : 3.00 (weighted total across all models)
  Models used:
    - gpt-4o: 2 calls
    - claude-sonnet-4-6: 1 calls

  All-time:
  Total API calls     : 47
  Total tokens        : 91820
  ...
============================================================
```

---

## Premium request weighting

GitHub Copilot bills some models as **premium requests** with a per-model multiplier.
The logger tracks the weighted total for each call and accumulates it in the summary.

Multipliers are sourced from the
[official GitHub Copilot documentation](https://docs.github.com/en/copilot/managing-copilot/monitoring-usage-and-entitlements/about-premium-requests).

| Model                                | Multiplier (paid plans) | Multiplier (Copilot Free) |
|--------------------------------------|-------------------------|---------------------------|
| Claude Haiku 4.5                     | 0.33                    | 1                         |
| Claude Opus 4.5                      | 3                       | Not applicable            |
| Claude Opus 4.6                      | 3                       | Not applicable            |
| Claude Opus 4.6 (fast mode, preview) | 30                      | Not applicable            |
| Claude Sonnet 4                      | 1                       | Not applicable            |
| Claude Sonnet 4.5                    | 1                       | Not applicable            |
| Claude Sonnet 4.6                    | 1                       | Not applicable            |
| Gemini 2.5 Pro                       | 1                       | Not applicable            |
| Gemini 3 Flash                       | 0.33                    | Not applicable            |
| Gemini 3 Pro                         | 1                       | Not applicable            |
| Gemini 3.1 Pro                       | 1                       | Not applicable            |
| GPT-4.1                              | 0 (included)            | 1                         |
| GPT-4o                               | 0 (included)            | 1                         |
| GPT-5 mini                           | 0 (included)            | 1                         |
| GPT-5.1                              | 1                       | Not applicable            |
| GPT-5.1-Codex                        | 1                       | Not applicable            |
| GPT-5.1-Codex-Mini                   | 0.33                    | Not applicable            |
| GPT-5.1-Codex-Max                    | 1                       | Not applicable            |
| GPT-5.2                              | 1                       | Not applicable            |
| GPT-5.2-Codex                        | 1                       | Not applicable            |
| GPT-5.3-Codex                        | 1                       | Not applicable            |
| GPT-5.4                              | 1                       | Not applicable            |
| GPT-5.4 mini                         | 0.33                    | Not applicable            |
| Grok Code Fast 1                     | 0.25                    | 1                         |
| Raptor mini                          | 0 (included)            | 1                         |
| Goldeneye                            | Not applicable          | 1                         |

Models with multiplier **0** are included in the base plan at no premium cost.
Models listed as **Not applicable** for free plan are not available on the Copilot Free tier.
Unknown models default to a multiplier of **1**.

The summary log shows the **weighted total** (sum of per-call multipliers), not a raw
request count.

---

## Files generated

| File                  | Description                                                         |
|-----------------------|---------------------------------------------------------------------|
| `ca.crt`              | Self-signed CA certificate — install as trusted root (Step 3)       |
| `ca.key`              | CA private key — keep private, never commit                         |
| `copiwilot_usage.log` | Append-only per-call log (raw, never overwritten)                   |
| `copilot_summary.log` | Human-readable summary, regenerated after every request             |
| `copilot_data.json`   | Persistent JSON store — accumulates stats across all runs and tasks |

Add `ca.key` to your `.gitignore`:

```bash
echo "ca.key" >> .gitignore
```

---

## Persistent data store

All stats are accumulated in `copilot_data.json` across runs. The file is structured as:

```json
{
  "global": {
    "total_calls": 12,
    "total_tokens": 48320,
    "cached_tokens": 8192,
    "reasoning_tokens": 1024,
    "premium_requests": 7.66,
    "models": { "claude-sonnet-4-6": 9, "gpt-4o": 3 },
    "first_seen": "2026-04-05 09:00:00",
    "last_seen":  "2026-04-05 17:30:00"
  },
  "tasks": {
    "my-feature": { "total_calls": 8, "..." : "..." },
    "sprint-42":  { "total_calls": 4, "..." : "..." }
  },
  "monthly": {
    "2026-03": { "total_calls": 5, "..." : "..." },
    "2026-04": { "total_calls": 7, "..." : "..." }
  }
}
```

The `monthly` map retains the **current month and the previous month only** — older
entries are pruned automatically on each flush. This gives you a rolling
month-over-month comparison without unbounded growth of the data file.

### Same task name on restart

When you start the proxy with a `-task` name that already has data, you are prompted:

```
Task "my-feature" already exists in copilot_data.json:
  calls=8  tokens=32100  premium=5.33  last seen=2026-04-05 14:22:01

[A]ggregate into existing task / [R]eset and start fresh / [C]ancel:
```

- **A** — new calls are added to the existing totals (default workflow).
- **R** — the task record is wiped and starts from zero.
- **C** — the proxy exits without starting.

---

## Requirements

- Go 1.21 or later (only needed for `go install` or building from source)
- The generated `ca.crt` trusted as a root CA on your machine (one-time, Step 3 — run `--trust-cert`)
