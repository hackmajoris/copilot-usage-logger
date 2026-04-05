# copilot-logger

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

## Installation

### Option 1: `go install` (requires Go)

```bash
go install github.com/hackmajoris/copilot-usage-logger@latest
```

The binary will be placed in `$(go env GOPATH)/bin/copilot-logger`. Make sure that directory is in your `PATH`.

### Option 2: Download a pre-built binary

Go to the [Releases page](https://github.com/hackmajoris/copilot-usage-logger/releases/latest) and download the archive for your OS and architecture:

| OS      | Architecture             | File                                       |
|---------|--------------------------|--------------------------------------------|
| macOS   | Apple Silicon (M1/M2/M3) | `copilot-usage-logger_darwin_arm64.tar.gz` |
| macOS   | Intel                    | `copilot-usage-logger_darwin_amd64.tar.gz` |
| Linux   | x86-64                   | `copilot-usage-logger_linux_amd64.tar.gz`  |
| Linux   | ARM64                    | `copilot-usage-logger_linux_arm64.tar.gz`  |
| Windows | x86-64                   | `copilot-usage-logger_windows_amd64.zip`   |
| Windows | ARM64                    | `copilot-usage-logger_windows_arm64.zip`   |

Extract and run:

```bash
# macOS / Linux
tar -xzf copilot-usage-logger_darwin_arm64.tar.gz
./copilot-logger

# Windows (PowerShell)
Expand-Archive copilot-usage-logger_windows_amd64.zip
.\copilot-logger.exe
```

Verify the download with `checksums.txt` (included in the release):

```bash
sha256sum --check checksums.txt
```

### Option 3: Build from source (requires Go)

```bash
git clone https://github.com/hackmajoris/copilot-usage-logger.git
cd copilot-usage-logger
go build -o copilot-logger copilot-logger.go
```

Or run directly without building:

```bash
go run copilot-logger.go
```

---

## Requirements

- Go 1.21 or later (only for `go install` or building from source)
- The generated `ca.crt` trusted as a root CA on your machine (one-time setup)

---

## CA trust setup (one-time)

Run the proxy once to generate `ca.crt`, then install it as a trusted root.

### macOS

```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain ca.crt
```

### Linux (Debian/Ubuntu)

```bash
sudo cp ca.crt /usr/local/share/ca-certificates/copilot-logger.crt
sudo update-ca-certificates
```

### Linux (RHEL/Fedora)

```bash
sudo cp ca.crt /etc/pki/ca-trust/source/anchors/copilot-logger.crt
sudo update-ca-trust
```

### Windows (PowerShell, run as Administrator)

```powershell
Import-Certificate -FilePath ca.crt -CertStoreLocation Cert:\LocalMachine\Root
```

---

## Usage

### Basic (default task, port 8080)

```bash
./copilot-logger
```

### Label a task

```bash
./copilot-logger -task my-feature
```

### Custom port and task

```bash
./copilot-logger -addr :9090 -task sprint-42
```

### View usage stats

```bash
# Current month summary
./copilot-logger --summary

# Previous month summary
./copilot-logger --prevmonth
```

### All flags

```bash
./copilot-logger \
  -addr   :8080             \
  -task   my-feature        \
  -log    copilot_usage.log \
  -data   copilot_data.json \
  -cacert ca.crt            \
  -cakey  ca.key
```

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:8080` | TCP address the MITM proxy listens on (e.g. `:8080` or `127.0.0.1:9090`) |
| `-task` | `default` | Label used to group token-usage stats in the summary log (e.g. `feature-branch` or `sprint-42`) |
| `-log` | `copilot_usage.log` | Path to the append-only NDJSON file that records every intercepted request and response |
| `-data` | `copilot_data.json` | Path to the persistent JSON store that accumulates stats across all runs |
| `-summary-file` | `copilot_summary.log` | Path to the summary file rewritten on each request with aggregated per-model token counts |
| `-cacert` | `ca.crt` | Path to the self-signed CA certificate used to intercept TLS traffic (created automatically on first run) |
| `-cakey` | `ca.key` | Path to the CA private key that signs per-host certificates (created automatically on first run — keep secret) |
| `--summary` | — | Print current-month usage summary and exit |
| `--prevmonth` | — | Print previous-month usage summary and exit |

---

## Docker container usage

If you run an agent or tool inside a Docker container and want it to go through the proxy, you need to:

1. Copy `ca.crt` (generated on first run) into your Docker build context.
2. Trust it in the image.
3. Point the container at the host proxy using `host.docker.internal` (Docker Desktop on Mac/Windows) or `172.17.0.1` (native Linux Docker).

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

Or if using a wrapper like `ralphex-dk`:

```bash
ralphex-dk \
  -E SSL_CERT_FILE=/usr/local/share/ca-certificates/copilot-logger.crt \
  -E HTTP_PROXY=http://host.docker.internal:8080 \
  -E HTTPS_PROXY=http://host.docker.internal:8080 \
  -E NO_PROXY=localhost,127.0.0.1
```

### host.docker.internal vs 172.17.0.1

| Environment                    | Host address to use                   |
|--------------------------------|---------------------------------------|
| Docker Desktop (Mac / Windows) | `host.docker.internal`                |
| Native Linux Docker            | `172.17.0.1` (default bridge gateway) |

On Docker Desktop for Mac, containers run inside a Linux VM and cannot reach the host at `172.17.0.1`. Use `host.docker.internal` instead — it always resolves to the correct host IP.

Verify the proxy is reachable before troubleshooting TLS:

```bash
docker run --rm alpine sh -c "apk add --no-cache netcat-openbsd && nc -zv host.docker.internal 8080"
```

---

## Proxy configuration

Point your HTTP and HTTPS proxy settings at the running proxy.

### Shell environment

```bash
export HTTP_PROXY=http://127.0.0.1:8080
export HTTPS_PROXY=http://127.0.0.1:8080
```

### VS Code (`settings.json`)

```json
"http.proxy": "http://127.0.0.1:8080",
"http.proxyStrictSSL": true
```

### GitHub Copilot extension proxy (VS Code `settings.json`)

```json
"github.copilot.advanced": {
  "debug.useNodeFetcher": true,
  "debug.chatOverrideProxyUrl": "http://127.0.0.1:8080"
}
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

Overwritten after every response with the latest aggregated stats. The MTD (month-to-date) section appears first, followed by all-time totals.

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

Multipliers are sourced from the [official GitHub Copilot documentation](https://docs.github.com/en/copilot/managing-copilot/monitoring-usage-and-entitlements/about-premium-requests).

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

The summary log shows the **weighted total** (sum of per-call multipliers), not a raw request count.

---

## Files generated

| File | Description |
|------|-------------|
| `ca.crt` | Self-signed CA certificate — install as trusted root |
| `ca.key` | CA private key — keep private, never commit |
| `copilot_usage.log` | Append-only per-call log (raw, never overwritten) |
| `copilot_summary.log` | Human-readable summary, regenerated from the JSON store after every request |
| `copilot_data.json` | Persistent JSON store — single source of truth, accumulates stats across all runs and tasks |

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
    "my-feature": { "total_calls": 8, ... },
    "sprint-42":  { "total_calls": 4, ... }
  },
  "monthly": {
    "2026-03": { "total_calls": 5, ... },
    "2026-04": { "total_calls": 7, ... }
  }
}
```

The `monthly` map retains the **current month and the previous month only** — older entries are pruned automatically on each flush. This gives you a rolling month-over-month comparison without unbounded growth of the data file.

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
<!---->

