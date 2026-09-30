# Design: gravwell-file-follow

This document explains the architecture and design decisions behind
`docker-log-watcher` and how it combines with `gravwell_file_follow` to provide
zero-touch, per-container log ingestion into Gravwell.

---

## Problem

Gravwell's `file_follow` ingester is excellent at tailing files — it tracks
offsets, handles log rotation, and ships entries efficiently. But using it for
Docker container logs has three friction points:

1. **Static config** — `[Follower]` blocks in `file_follow.conf` are written by
   hand. Every new container requires a manual config change and a restart.

2. **Permission race** — Docker creates container log directories with mode
   `0710` (owner: root). `file_follow` receives an inotify event for the new
   directory immediately but cannot read it. It logs an error and **never
   retries** — the container's logs are silently dropped until the next restart.

3. **No per-container tagging** — Without manual config, all containers land in
   the same Gravwell tag, making it impossible to query a single service without
   filtering on container name inside the payload.

---

## Solution

A small Go daemon — `docker-log-watcher` — runs alongside `gravwell_file_follow`
inside a single privileged container. It:

- Watches the Docker socket for container lifecycle events
- Generates a `[Follower]` config stanza for each running container
- Writes those stanzas as overlay files into `file_follow.conf.d/`
- Supervises `gravwell_file_follow` as a child process, restarting it when
  configs change
- Respects `gravwell.skip` and `gravwell.tag` labels on containers

Because the container runs as root with the host root filesystem bind-mounted
read-only (with `rslave` propagation), it always has permission to read any
container's log files — even when Docker's data root lives on a separate mount
point (e.g. `/home/docker_home`). The permission race is eliminated entirely.
The actual container log path is auto-detected at startup by querying the Docker
socket for `DockerRootDir`.

---

## Architecture

```
gravwell-file-follow container
┌─────────────────────────────────────────────────────────────┐
│                                                             │
│  tini (PID 1)                                               │
│    └─ docker-log-watcher                                    │
│         ├─ watches /var/run/docker.sock                     │
│         ├─ writes /opt/gravwell/etc/file_follow.conf.d/     │
│         └─ supervises gravwell_file_follow (child process)  │
│                  └─ reads conf.d/ + ships to Gravwell       │
│                                                             │
│  Bind mounts (read-only):                                   │
│    /var/run/docker.sock   — Docker API                      │
│    /host (rslave)         — host root; logs auto-detected   │
│                                                             │
│  Volume (read-write):                                       │
│    /opt/gravwell/etc             — state file, conf.d/      │
└─────────────────────────────────────────────────────────────┘
                          │ TLS ingest
                          ▼
                    Gravwell indexer
```

### Supervisor pattern

`docker-log-watcher` spawns `gravwell_file_follow` as a child process rather
than running them independently. This means:

- The watcher knows exactly when `file_follow` is running
- Config updates trigger a clean SIGTERM → wait → restart cycle
- Crashes are detected and the child is respawned automatically
- A single SIGTERM to the container stops both processes cleanly
- No PID files, no shell scripts, no race conditions

`tini` sits at PID 1 to handle zombie reaping — important since we are forking a
child inside a container.

### Debouncing

When a whole compose stack comes up simultaneously, many `start` events fire in
quick succession. Rather than restarting `file_follow` for each one, the watcher
waits for a configurable quiet period (default: 2 seconds) after the last event
before writing configs and restarting. A stack of 10 containers triggers exactly
one restart.

### Docker reconnect

If the Docker API stream drops (daemon restart, socket hiccup), the watcher
reconnects with exponential backoff (2s → 4s → 8s … up to 60s), then performs a
full re-sync to catch any containers that started while disconnected.

### Destroy drain delay

When a container is destroyed (including `--rm` one-shot containers), the
watcher does **not** remove its config immediately.  Instead it waits a
configurable grace period (`WATCHER_DESTROY_DELAY_SECS`, default 30s) so that
`file_follow` has time to read any remaining log data from the container's log
file.  This is essential for short-lived batch jobs that produce output right
before exiting.

If the same container name starts again before the timer fires (e.g. a
recreate), the pending removal is cancelled and the config is updated in place.
Setting `WATCHER_DESTROY_DELAY_SECS=0` restores immediate removal.

---

## Container Labels

Three labels control per-container behaviour:

| Label | Value | Effect |
|---|---|---|
| `gravwell.enable` | `"true"` | Opt this container in. Required when `WATCHER_OPT_IN=true`; ignored in default opt-out mode. |
| `gravwell.skip` | `"true"` | Exclude this container from ingestion entirely. Always wins — overrides `gravwell.enable`. |
| `gravwell.tag` | any string | Override the auto-derived Gravwell tag name |

**Opt-out mode (default, `WATCHER_OPT_IN=false`):** every container is ingested
unless it carries `gravwell.skip=true` or matches an `WATCHER_EXCLUDE_NAMES`
prefix. No labels are required.

**Opt-in mode (`WATCHER_OPT_IN=true`):** containers are skipped unless they
carry `gravwell.enable=true`. Useful in mixed environments where only some
containers should be ingested. `gravwell.skip=true` and `WATCHER_EXCLUDE_NAMES`
still apply.

```yaml
# Opt-in mode — only ingest this container
labels:
  gravwell.enable: "true"

# Skip a container (works in either mode)
labels:
  gravwell.skip: "true"

# Override the tag for a container with a long compose-generated name
labels:
  gravwell.tag: "nginx"
```

For bulk exclusions without labelling every container, set the
`WATCHER_EXCLUDE_NAMES` environment variable on the watcher container.  It
accepts a comma-separated list of **name prefixes** — any container whose name
starts with a listed prefix is silently skipped:

```yaml
environment:
  # Exact name match
  WATCHER_EXCLUDE_NAMES: "portainer,traefik"

  # Prefix match — skips buildx_buildkit_builder-0, buildx_buildkit_builder-abc, etc.
  WATCHER_EXCLUDE_NAMES: "buildx_buildkit"
```

---

## Tag Naming

When no `gravwell.tag` label is set, the tag is derived from the container name:

1. Strip leading `/` (Docker convention)
2. Replace any character that is not `[a-zA-Z0-9_-]` with `_`

Examples: `my-app` → `my-app`, `nocodb-nocodb-1` → `nocodb-nocodb-1`,
`My.Service` → `My_Service`

---

## Generated Config Format

Each running container gets a file in `file_follow.conf.d/`:

```ini
# Auto-generated by docker-log-watcher — DO NOT EDIT
# Container: myapp (abc123def456...)
# Generated: 2026-02-22T02:12:44Z

[Follower "myapp"]
    Base-Directory="/host/var/lib/docker/containers/abc123def456..."
    File-Filter="*-json.log"
    Tag-Name=myapp
    Assume-Local-Timezone=false
    Ignore-Timestamps=false
    Recursive=false
```

`gravwell_file_follow` is started with `-config-overlays` pointing at this
directory, so it merges these stanzas with the base config at startup.

Config files are written atomically (write to `.tmp`, then `os.Rename`) so
`file_follow` never reads a partial file.

---

## Docker Log Format

Docker's `json-file` log driver wraps each log line in a JSON envelope:

```json
{"log":"actual message here\n","stream":"stdout","time":"2026-02-22T02:08:42.196Z"}
```

The `log` field is the raw payload — often itself JSON-escaped if the
application emits structured logs. `file_follow` parses the timestamp from the
outer `time` field and stores the full envelope as the entry.

In Gravwell queries, unwrap with:

```
tag=myapp json log | unescape log | json -e log
```

Or define a macro (System → Macros):

```
Name:      DOCKER
Expansion: json log | unescape log | json -e log
```

Then queries become: `tag=myapp $DOCKER timestamp level message`

---

## Build

Three-stage Dockerfile:

| Stage | Base | Output |
|---|---|---|
| `watcher-builder` | `golang:1.24-bookworm` | `docker-log-watcher` static binary |
| `ff-builder` | `golang:1.24-bookworm` | `gravwell_file_follow` static binary + `tini` |
| `runtime` | `busybox:stable` | Minimal image with both binaries |

Both Go binaries are compiled with `CGO_ENABLED=0` for fully static binaries.
`GOARCH=${TARGETARCH}` is set explicitly so cross-compilation works when building
`arm64` on an `amd64` host via Docker Buildx. Supports `linux/amd64` and
`linux/arm64`.

`tini` is downloaded from GitHub releases using `${TARGETARCH}` to ensure the
correct binary for each platform.

### Custom CA

If your Gravwell indexer uses a private/self-signed TLS certificate, pass the CA
PEM at build time:

```bash
docker buildx build --build-arg CUSTOM_CA=my-ca.crt -t myrepo/gravwell-file-follow .
```

Without `CUSTOM_CA`, the image uses the standard Debian CA bundle only.

---

## Static Host Log Sources

In addition to container logs, `file_follow` can ingest arbitrary log files from
the host — syslog, application logs, audit logs, etc.  Because the entire host
root is already visible at `/host` via the `rslave` bind mount, no additional
mounts are needed for the log files themselves.

To avoid the watcher cleaning up hand-crafted configs, place them in a separate
directory and point `WATCHER_STATIC_CONF_DIR` at it:

```ini
# static-configs/host-syslog.conf
[Follower "host-syslog"]
    Base-Directory="/host/var/log"
    File-Filter="syslog"
    Tag-Name=host-syslog
    Assume-Local-Timezone=true
    Ignore-Timestamps=false
    Recursive=false
```

```yaml
# docker-compose.yml
volumes:
  - /opt/static-gravwell-configs:/opt/gravwell/etc/static.conf.d:ro
environment:
  WATCHER_STATIC_CONF_DIR: "/opt/gravwell/etc/static.conf.d"
```

The watcher passes this directory to `gravwell_file_follow` as a second
`-config-overlays` argument and never reads, writes, or deletes files in it.

---

## Future Enhancements

- **Health check endpoint** — HTTP `/healthz` for Docker/Portainer health checks
- **Prometheus metrics** — containers tracked, restarts, ingestion errors
- **Config validation** — run `gravwell_file_follow -validate` before restarting
- **Graceful drain** — keep config for N minutes after container die to finish
  tailing any remaining buffered log lines before removal
- **Multi-host tagging** — optional hostname prefix on tag names for multi-host
  Gravwell deployments

---

## What This Is Not

- **Not a log shipper** — no transformation, buffering, or routing. It generates
  config files for `gravwell_file_follow` and gets out of the way.
- **Not a replacement for the Gravwell Docker log driver** — that approach
  requires changing `--log-driver` on every container. This approach requires
  zero changes to other containers.
- **Not a general-purpose solution** — targets the `json-file` log driver
  specifically (Docker's default). Containers using `syslog`, `journald`, or
  other drivers are not covered.
