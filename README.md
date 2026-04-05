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

## Requirements

- Go 1.21 or later
- The generated `ca.crt` trusted as a root CA on your machine (one-time setup)

---

## Build

```bash
go build -o copilot-logger copilot-logger.go
```

Or run directly without building:

```bash
go run copilot-logger.go
```

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

### All flags

```bash
./copilot-logger \
  -addr    :8080               \  # proxy listen address
  -task    my-feature          \  # task label for grouping
  -log     copilot_usage.log   \  # append-only request/response log
  -summary copilot_summary.log \  # overwritten on every response
  -cacert  ca.crt              \  # CA certificate (created if missing)
  -cakey   ca.key                 # CA private key  (created if missing)
```

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

| Environment | Host address to use |
|---|---|
| Docker Desktop (Mac / Windows) | `host.docker.internal` |
| Native Linux Docker | `172.17.0.1` (default bridge gateway) |

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

Overwritten after every response with the latest aggregated stats.

```
============================================================
COPILOT USAGE SUMMARY  (updated 2026-04-05 14:01:03)
============================================================
  Total API calls     : 3
  Total tokens        : 5210
  Cached tokens       : 1024
  Reasoning tokens    : 400
  Premium requests    : 3 (3x weight; 1 raw requests)
  Models used:
    - gpt-4o: 2 calls
    - o3-mini: 1 calls

  Per-task breakdown:
  --------------------------------------------------------
  Task: my-feature
    Calls           : 2
    Total tokens    : 3200
    ...
============================================================
```

---

## Premium request weighting

Any response that contains `reasoning_tokens > 0` is counted as **3 premium
requests** (matching GitHub Copilot's published billing weight for reasoning
models). The summary shows both the weighted total and the raw request count.

---

## Files generated

| File | Description |
|---|---|
| `ca.crt` | Self-signed CA certificate — install as trusted root |
| `ca.key` | CA private key — keep private, never commit |
| `copilot_usage.log` | Append-only per-call log |
| `copilot_summary.log` | Latest aggregated summary (overwritten) |

Add `ca.key` to your `.gitignore`:

```bash
echo "ca.key" >> .gitignore
```
