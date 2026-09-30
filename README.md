# gravwell-file-follow

A self-contained container that automatically discovers running containers on the host — whether managed by **Docker** or **Podman** — and ingests their log files into [Gravwell](https://www.gravwell.io/) — one tag per container, updated dynamically as containers start and stop.

## How It Works

Two binaries run inside the container:

| Binary | Role |
|---|---|
| `docker-log-watcher` | Custom Go daemon (this repo). Watches container runtime events via the socket (Docker or Podman), generates per-container `[Follower]` config stanzas, and manages the `gravwell_file_follow` child process. |
| `gravwell_file_follow` | Official Gravwell file ingester, compiled from source. Reads the generated configs and ships log lines. |

### Container Discovery Flow

1. On startup, `docker-log-watcher` enumerates all running containers and writes a `file_follow.conf.d/<name>.conf` overlay for each one.
2. It then starts `gravwell_file_follow` as a supervised child process (PID forwarding, crash respawn).
3. When a container starts or stops, the watcher debounces the event, rewrites the overlay configs, and sends `SIGHUP` (or restarts) `gravwell_file_follow` to pick up the changes.
4. Containers are skipped if they have a `gravwell.skip: "true"` label, match a `WATCHER_EXCLUDE_NAMES` prefix, or are the watcher container itself.

### Tag Naming

- Containers with a `gravwell.tag` label use that value verbatim.
- Otherwise the tag is derived from the container name: lowercased, non-alphanumeric characters replaced with `-`, leading/trailing `-` stripped. For example `nocodb-nocodb-1` → `nocodb-nocodb-1`.

---

## Architecture

- **Multi-stage Dockerfile**: `watcher-builder` (Go) → `ff-builder` (Go + tini) → `busybox:stable` runtime
- **Multi-arch**: `linux/amd64` and `linux/arm64` via Docker Buildx
- **Gravwell version**: `v3.8.76` (open-source tag; built from source)
- **tini v0.19.0**: Downloaded from GitHub releases using `${TARGETARCH}` — always arch-correct
- **State volume** at `/opt/gravwell/etc`: Persists file offsets and state across container restarts
- **Config template** at `/etc/gravwell/file_follow.conf.template`: Lives *outside* the state volume mount so it is always the image-baked version

---

## Quick Start

### 1. Prerequisites

- Docker with Buildx (for multi-arch builds) or standard Docker (for native builds)
- Access to a Gravwell indexer
- The external Docker network used by your other compose stacks (default: `gravwell_file_follow_default`)

### 2. Configure Environment

Copy the example and fill in your values:

```bash
cp .env.example .env
```

```bash
# .env
GRAVWELL_INGEST_SECRET=YourSecretHere

# Use one or both target types:
GRAVWELL_CLEARTEXT_TARGETS=gravwell.example.com:4023
GRAVWELL_ENCRYPTED_TARGETS=gravwell.example.com:4024

# Set to false if using a trusted/public TLS certificate
INSECURE_SKIP_TLS_VERIFY=true
```

### 3. Build and Run

```bash
docker compose build
docker compose up -d
docker compose logs -f
```

---

## Configuration Reference

### Gravwell Ingester Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GRAVWELL_INGEST_SECRET` | ✅ | — | Must match the Gravwell server's ingest secret |
| `GRAVWELL_CLEARTEXT_TARGETS` | one of these | — | Comma-separated `host:port` for plain TCP |
| `GRAVWELL_ENCRYPTED_TARGETS` | one of these | — | Comma-separated `host:port` for TLS |
| `INSECURE_SKIP_TLS_VERIFY` | | `true` | Skip TLS cert verification (set `false` for public certs) |
| `GRAVWELL_LOG_LEVEL` | | `INFO` | Gravwell log verbosity: `INFO`, `WARN`, `ERROR`, `OFF` |
| `GRAVWELL_INGESTER_UUID` | | _(auto-generated)_ | Stable UUID for this ingester — survives redeploys. Leave blank to auto-generate and persist on the state volume |
| `GRAVWELL_INGESTER_LABEL` | | `docker-file-follow` | Human-friendly label shown in Gravwell's Systems & Health page |

### Watcher Variables

| Variable | Default | Description |
|---|---|---|
| `WATCHER_BACKEND` | `docker` | Container runtime backend. Set to `podman` to watch a Podman node. Controls socket path, container directory layout, and default log file glob. |
| `WATCHER_SOCKET` | _(backend default)_ | Override the Unix socket path. Docker default: respects `DOCKER_HOST` / `/var/run/docker.sock`. Podman default: `/run/podman/podman.sock`. Use this for rootless Podman (e.g. `/run/user/1000/podman/podman.sock`). |
| `WATCHER_CONF_DIR` | `/opt/gravwell/etc/file_follow.conf.d` | Where per-container overlay configs are written |
| `WATCHER_CONTAINER_DIR` | _(auto-detected)_ | Container log directory. Detected at startup via the runtime socket. Docker: `<graphRoot>/containers`. Podman: `<graphRoot>/overlay-containers`. Override only if auto-detection fails. |
| `WATCHER_DEFAULT_TAG` | _(backend name)_ | Fallback Gravwell tag when the container name is empty. Defaults to `docker` or `podman` based on `WATCHER_BACKEND`. |
| `WATCHER_SELF_CONTAINER` | `gravwell-file-follow` | Watcher's own container name (always excluded) |
| `WATCHER_DEBOUNCE_SECS` | `2` | Seconds to wait after last event before restarting `file_follow` |
| `WATCHER_DESTROY_DELAY_SECS` | `30` | Seconds to keep a container's config after destroy, giving `file_follow` time to drain remaining logs (set `0` for immediate removal) |
| `WATCHER_EXCLUDE_NAMES` | `buildx_buildkit` | Comma-separated container name **prefixes** to always skip (e.g. `buildx_buildkit` matches `buildx_buildkit_builder-0`) |
| `WATCHER_LOG_LEVEL` | `info` | Watcher log verbosity: `debug`, `info`, `warn`, `error` |
| `WATCHER_OPT_IN` | `false` | Set to `true` to switch to opt-in mode: containers are skipped unless they carry `gravwell.enable=true`. `gravwell.skip=true` and `WATCHER_EXCLUDE_NAMES` still apply in either mode. |
| `WATCHER_STATIC_CONF_DIR` | _(unset)_ | Path inside the container to a directory of static `[Follower]` configs for non-container log sources (host syslog, app logs, etc.). Passed to `file_follow` as an additional `-config-overlays` directory. The watcher never modifies files here — it is entirely user-managed. |

### Container Labels

Add these labels to any container you want to control:

| Label | Description |
|---|---|
| `gravwell.enable` | `"true"` — opt this container in (required when `WATCHER_OPT_IN=true`) |
| `gravwell.skip` | `"true"` — exclude this container entirely (wins over `gravwell.enable`) |
| `gravwell.tag` | Override the auto-derived Gravwell tag name |
| `gravwell.log-dir` | Override the `Base-Directory` for this container's follower config. Use this when logs are written to a named volume rather than the default Docker log path. The path must be the location **inside the watcher container** where the volume is mounted. |
| `gravwell.log-filter` | Override the `File-Filter` glob. Default is `*-json.log` for Docker and `*.log` for Podman. Use this when logs are not in the runtime's standard format — e.g. `app-*.log`. |
| `gravwell.persist` | `"true"` — keep the follower config alive after the container is destroyed. The config is rewritten on the next start of a same-named container and is only removed by setting `gravwell.persist: "false"` (or removing the label) on a subsequent run. Typically used together with `gravwell.log-dir` so that a named-volume log source is read continuously even when no container is running. |
| `gravwell.persist-ttl` | Go duration string (e.g. `"24h"`, `"168h"`, `"30m"`) — automatically remove the persisted config this long after the container is last destroyed. The countdown starts when the destroy event is processed (after the drain delay) and is stored on disk so it survives watcher restarts. Cancelled if the container restarts before the TTL fires. Has no effect unless `gravwell.persist: "true"` is also set. |
| `gravwell.timestamp-regex` | RE2 regex to locate the timestamp string within each log line. Must be set together with `gravwell.timestamp-format`. When omitted, `file_follow` uses its built-in auto-detection. |
| `gravwell.timestamp-format` | Go time layout string (e.g. `"2006-01-02 15:04:05.999999-07:00"`) matching the text captured by `gravwell.timestamp-regex`. Must be set together with `gravwell.timestamp-regex`. |

```yaml
labels:
  gravwell.enable: "true"        # Opt this container in (required when WATCHER_OPT_IN=true)
  gravwell.skip: "true"          # Exclude this container entirely (wins over gravwell.enable)
  gravwell.tag: "my-custom-tag"  # Override the auto-derived tag name
  gravwell.log-dir: "/mnt/app-logs"   # Read logs from a named volume instead of Docker's log path
  gravwell.log-filter: "*.log"        # Match non-JSON log files in the volume
  gravwell.persist: "true"            # Keep config after container destroy (use with log-dir)
  gravwell.persist-ttl: "168h"        # Auto-remove after 7 days with no container restart
  gravwell.timestamp-regex: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+[+-]\d{2}:\d{2}'
  gravwell.timestamp-format: "2006-01-02 15:04:05.999999-07:00"
```

#### Ingesting logs from a named volume (continuous)

If your container writes logs to a named Docker volume, mount the volume into the watcher container (read-only) and combine `gravwell.log-dir` with `gravwell.persist`. The follower config stays in place between container runs so `file_follow` reads the volume without interruption:

```yaml
# docker-compose.yml — your application
services:
  myapp:
    image: myapp:latest
    volumes:
      - app-logs:/var/log/myapp
    labels:
      gravwell.log-dir: "/mnt/myapp-logs"
      gravwell.log-filter: "*.log"
      gravwell.persist: "true"
      gravwell.persist-ttl: "168h"    # clean up if container is gone for 7+ days

volumes:
  app-logs:
```

```yaml
# docker-compose.yml — gravwell-file-follow
services:
  gravwell-file-follow:
    volumes:
      - app-logs:/mnt/myapp-logs:ro   # same volume, read-only

volumes:
  app-logs:
    external: true
```

> **Note:** Without `gravwell.persist-ttl`, a persisted config is never automatically expired. If the container name is retired permanently without a TTL, remove the config manually:
> ```bash
> docker exec gravwell-file-follow rm \
>   /opt/gravwell/etc/file_follow.conf.d/myapp.conf \
>   /opt/gravwell/etc/file_follow.conf.d/myapp.persist
> ```
> Then send `SIGHUP` to the watcher (or restart the container) to reload.

---

## Ingesting Host Log Files

To ingest logs from the Docker host itself (syslog, app logs, etc.) alongside container logs, use `WATCHER_STATIC_CONF_DIR`. The watcher passes this directory to `file_follow` as an extra `-config-overlays` source but **never reads or modifies** the files in it.

### 1. Create your static config

```ini
# /path/on/host/static-configs/host-syslog.conf
[Follower "host-syslog"]
    Base-Directory="/host/var/log"
    File-Filter="syslog"
    Tag-Name=host-syslog
    Assume-Local-Timezone=true
    Ignore-Timestamps=false
    Recursive=false
```

Note: paths use `/host/...` because the host root is bind-mounted at `/host` inside the container.

### 2. Mount the directory and set the env var

```yaml
# docker-compose.yml
volumes:
  - /path/on/host/static-configs:/opt/gravwell/etc/static.conf.d:ro

environment:
  WATCHER_STATIC_CONF_DIR: "/opt/gravwell/etc/static.conf.d"
```

### 3. Query in Gravwell

```
tag=host-syslog
```

---

## Podman Deployment

Set `WATCHER_BACKEND=podman` to watch a Podman node.  Podman exposes a Docker-compatible REST API so no code changes are needed in your containers — all `gravwell.*` labels work identically.

### What changes automatically

| | Docker | Podman |
|---|---|---|
| Socket | `/var/run/docker.sock` | `/run/podman/podman.sock` |
| Container log parent | `<graphRoot>/containers/<id>/` | `<graphRoot>/overlay-containers/<id>/userdata/` |
| Default log file glob | `*-json.log` | `*.log` |
| Default tag | `docker` | `podman` |

### Log driver requirement

Podman containers must use the `k8s-file` (or `json-file`) log driver for file-based ingestion.  Containers using `journald` (common on systemd hosts) produce no log files on disk.  Set the driver per-container or globally in `/etc/containers/containers.conf`:

```ini
[containers]
log_driver = "k8s-file"
```

Or per-container in your compose file:

```yaml
services:
  myapp:
    logging:
      driver: k8s-file
```

### Root Podman (systemd service)

```yaml
# docker-compose.yml / podman-compose.yml
services:
  gravwell-file-follow:
    image: cybercinch/gravwell-file-follow:latest
    privileged: true
    user: "0:0"
    environment:
      WATCHER_BACKEND: podman
      GRAVWELL_INGEST_SECRET: YourSecretHere
      GRAVWELL_CLEARTEXT_TARGETS: gravwell.example.com:4023
    volumes:
      - /run/podman/podman.sock:/var/run/docker.sock:ro   # mount at the default path so entrypoint needs no change
      - /:/host:ro,rslave
      - file_follow_state:/opt/gravwell/etc
      - /opt/gravwell/log:/opt/gravwell/log

volumes:
  file_follow_state:
```

> **Tip:** Mounting the Podman socket at `/var/run/docker.sock` avoids needing to set `WATCHER_SOCKET`.  Alternatively, mount it anywhere and set `WATCHER_SOCKET=/path/to/podman.sock`.

### Rootless Podman

Rootless Podman sockets live under the user's runtime directory.  Replace `<UID>` with the UID of the Podman user (find it with `id -u`):

```yaml
environment:
  WATCHER_BACKEND: podman
  WATCHER_SOCKET: /run/user/<UID>/podman/podman.sock

volumes:
  - /run/user/<UID>/podman/podman.sock:/run/user/<UID>/podman/podman.sock:ro
  - /:/host:ro,rslave
```

> **Note:** Rootless Podman stores container data under the user's home directory (e.g. `~/.local/share/containers/storage`).  The watcher auto-detects this via the socket; set `WATCHER_CONTAINER_DIR` explicitly only if auto-detection fails.

---

## docker-compose.yml Notes

The compose file expects:

- **External network**: `gravwell_file_follow_default` — update `networks.gravwell.name` to match your environment.
- **State volume**: Created fresh on first run; persists file offsets across restarts.
- **`privileged: true` / `user: "0:0"`**: Required to read container log files (both Docker and Podman write them as root for system-level runtimes).
- **Host root bind mount with `rslave` propagation**: The entire host filesystem is mounted at `/host` read-only. `rslave` propagation ensures sub-mounts (e.g. a data root on a separate partition like `/home/docker_home`) are visible inside the container. The watcher auto-detects the graph root via the runtime socket at startup.
- **Podman socket**: Mount `/run/podman/podman.sock` in place of (or alongside) the Docker socket, or set `WATCHER_SOCKET` to its path explicitly.

---

## Volumes & Paths

| Path | Description |
|---|---|
| `/var/run/docker.sock` | Docker API socket (read-only). For Podman, mount the Podman socket here or set `WATCHER_SOCKET` to its path. |
| `/run/podman/podman.sock` | Podman root socket (read-only). Mount here and set `WATCHER_SOCKET=/run/podman/podman.sock`, or mount at `/var/run/docker.sock` to avoid setting the env var. |
| `/host` (rslave) | Host root filesystem (read-only). Watcher reads container logs from `/host<graphRoot>/containers/` (Docker) or `/host<graphRoot>/overlay-containers/` (Podman). `rslave` propagation exposes all host sub-mounts. |
| `/opt/gravwell/etc` | State volume: `file_follow.conf`, `file_follow.state`, `file_follow.conf.d/` |
| `/opt/gravwell/log` | Gravwell ingester logs |
| `/etc/gravwell/file_follow.conf.template` | Image-baked config template (outside state volume) |

---

## Troubleshooting

### Check ingester logs

```bash
docker exec gravwell-file-follow tail -50 /opt/gravwell/log/file_follow.log
```

Look for `Connected` / `authenticated` messages. A `bad secret` error means `GRAVWELL_INGEST_SECRET` doesn't match.

### List generated overlay configs

```bash
docker exec gravwell-file-follow ls /opt/gravwell/etc/file_follow.conf.d/
docker exec gravwell-file-follow cat /opt/gravwell/etc/file_follow.conf.d/my-container.conf
```

### View rendered base config

```bash
docker exec gravwell-file-follow cat /opt/gravwell/etc/file_follow.conf
```

### Reset state (re-ingest everything)

```bash
docker compose down
docker volume rm gravwell-file-follow_file_follow_state
docker compose up -d
```

---

## Building Multi-arch Images

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg GRAVWELL_VERSION=v3.8.76 \
  -t cybercinch/gravwell-file-follow:latest \
  --push .
```

A GitHub Actions workflow (`.github/workflows/release.yml`) runs tests on every push and builds/pushes the multi-arch image on version tags (`v*`).

---

## Project Structure

```
├── Dockerfile                   # Multi-stage multi-arch build
├── entrypoint.sh                # Renders config from template, execs docker-log-watcher
├── file_follow.conf.template    # Base Gravwell config (no [Follower] blocks)
├── docker-compose.yml
├── .env.example
└── watcher/                     # docker-log-watcher Go module
    ├── go.mod
    ├── cmd/docker-log-watcher/
    │   └── main.go              # Entry point, signal handling
    └── internal/
        ├── config/              # WATCHER_* env var parsing
        ├── docker/              # Container runtime event loop + debounce + reconnect (Docker & Podman)
        ├── follower/            # Config generation, tag/name sanitisation
        └── supervisor/          # Child process lifecycle + crash respawn
```

---

## License

`docker-log-watcher` source code is MIT licensed.  
Gravwell software is proprietary — see [gravwell.io/eula](https://www.gravwell.io/eula) for terms.

