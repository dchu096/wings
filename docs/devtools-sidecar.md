# Locked-down DevTools Sidecar (Axis)

**Status:** v1 contract + daemon implementation  
**Audience:** Atlas Panel integration + Axis node operators  
**Contract version:** `devtools.v1`

Axis owns a heavily locked-down **per-server sidecar container** that shares the game data volume. Developers attach a real terminal (web PTY now; optional SSH gateway later) to that sandbox only — never to the game container or the Axis host.

This is **not** the game console (process stdin/stdout). It is a **developer terminal**.

---

## Goals

| Allow | Deny |
|--------|------|
| Allowlisted tools: `git`, `curl`, `wget`, `unzip`, `tar`, `nano`, `vim`, `less`, `jq`, `ca-certificates` | Unrestricted package install, compilers, Docker CLI |
| Read/write server files via shared data volume (`/home/container`) | Docker socket, privileged mode, host mounts beyond server data |
| Network egress for git/curl (node Docker network) | Joining the **game** container network namespace |
| | Executing user-downloaded binaries/scripts **from the sidecar** (`noexec,nodev,nosuid` on data + `/tmp`) |

### Residual risk (honest)

`noexec` on the data mount stops execution **inside the sidecar**. After a game restart, the **game container** can still load jars/plugins/scripts that were written through DevTools or SFTP. DevTools is not an anti-malware boundary for the game process.

---

## Decisions (v1)

1. **Start-on-attach** — when `devtools_enabled` is true, the sidecar is created/started on first PTY attach (or explicit start). Idle timeout stops it when unused.
2. **Separate network** — sidecar joins the normal Axis/Docker bridge (`Docker.Network.Mode`), **not** `container:<game>`. Outbound egress yes; game ports are not shared into the sandbox.
3. **Read-only rootfs + tmpfs** — image root is read-only; `/tmp` is tmpfs with `noexec,nosuid,nodev`. Writable work is the data bind at `/home/container`.
4. **No git credentials / SSH agent forwarding in v1** — users pass tokens in remotes/env themselves if needed; Axis does not inject secrets.
5. **One global image** — `system.devtools.image` in `config.yml`. Optional per-server override is reserved (`devtools_image` in panel settings) but not required for Atlas v1.
6. **Central SSH proxy** (`dchu096.me` etc.) is **phase 2** — Axis exposes a local attach hook only; bastion authenticates via panel then hops here.

---

## Lifecycle

```
Panel sets devtools_enabled ──sync──► Axis Configuration
                                         │
                    enabled=false ────────┼──► Destroy sidecar (if any)
                    enabled=true ─────────┼──► Mark ready; start on attach
                                         │
PTY attach / POST start ─────────────────┼──► Ensure image → Create → Start
Idle timeout (no attach) ────────────────┼──► Stop container (keep for fast restart)
Server suspend / delete / transfer out ──┼──► Stop + Destroy
Install / reinstall ─────────────────────┼──► Stop for duration; may restart after if still enabled
Backup / restore ────────────────────────┼──► No special coupling (files shared; avoid attach during restore)
```

| Event | Behavior |
|--------|----------|
| `devtools_enabled` → true | Feature armed; container not necessarily running |
| `devtools_enabled` → false | Stop + remove sidecar |
| First attach while enabled | Pull image if needed, create, start, `docker exec` PTY |
| Idle > `idle_timeout` | Stop container |
| Server deleted | Destroy sidecar before game environment destroy |
| Suspended | Destroy/stop sidecar; attach fails closed |
| Image missing | Status `image_missing`; attach returns clear error |

Container name: `{server_uuid}_devtools`

---

## Security boundary

### Image allowlist (shipped tools)

Defined by the sidecar image (see `devtools/Dockerfile`):

- `bash` (login shell for PTY — locked down by mount flags + no package manager as root workflow)
- `git`, `curl`, `wget`
- `unzip`, `tar`, `gzip`
- `nano`, `vim`, `less`
- `jq`, `ca-certificates`, `openssh-client` (for `git+ssh` **user-supplied** keys in the data dir only)

**Not** included: `docker`, `gcc`/`g++`, `make`, `python3` toolchains beyond what a minimal base needs, package managers usable to escalate (image is distroless-ish / no `apt` in default PATH for the runtime user — Alpine/busybox variants may still have `apk` present; treat host policy + non-root UID as the control, and prefer a custom image without package managers for production).

### Container hardening

- User: same UID/GID as game containers (`system.user.uid/gid`)
- `ReadonlyRootfs: true`
- `SecurityOpt: no-new-privileges`
- `CapDrop: ALL` (stricter than game containers)
- **No** `Privileged`, **no** docker.sock mount
- Data volume: local driver bind of server data → `/home/container` with `o=bind,rw,noexec,nosuid,nodev` (Docker rejects those flags on plain bind `mode` strings)
- `/tmp` tmpfs: `rw,noexec,nosuid,nodev,size=<tmpfs_size_mb>m`
- Resource caps from `system.devtools` (memory/CPU/PIDs)
- Labels: `Service=Axis`, `ContainerType=devtools_sidecar`

### Auth

- HTTP control APIs: Wings/Axis node token (`Authorization` middleware) — panel → daemon
- PTY websocket: server JWT (same family as `/api/servers/:server/ws`) with permission **`devtools.console`**
- Fail closed: feature disabled globally, server suspended, `devtools_enabled=false`, or image missing → attach errors with an explicit reason

---

## API contract (`devtools.v1`)

Base path: `/api/servers/:server/devtools`

### Status — `GET /api/servers/:server/devtools`

**Auth:** node token  

**Response `200`:**

```json
{
  "contract": "devtools.v1",
  "enabled": true,
  "feature_available": true,
  "state": "running",
  "container": {
    "name": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx_devtools",
    "id": "abc123..."
  },
  "image": "ghcr.io/dchu096/atlas-devtools:latest",
  "error": null,
  "idle_timeout_seconds": 900,
  "tools": ["git", "curl", "wget", "unzip", "tar", "nano", "vim", "less", "jq"]
}
```

**`state` enum:** `disabled` | `stopped` | `starting` | `running` | `error` | `image_missing`

### Enable / disable — `POST /api/servers/:server/devtools`

**Auth:** node token  

**Request:**

```json
{ "enabled": true }
```

**Response `200`:** same body as GET status after reconcile.

Panel should persist `devtools_enabled` and include it on the next server settings sync. This endpoint applies immediately on the node.

### Start / stop (optional ops) — `POST /api/servers/:server/devtools/power`

**Auth:** node token  

```json
{ "action": "start" }
```

`action`: `start` | `stop` | `destroy`

### PTY attach — `GET /api/servers/:server/devtools/ws`

**Auth:** JWT over websocket (same pattern as console WS)  

**Upgrade:** WebSocket  

**Client → Axis**

| Event | Args | Notes |
|--------|------|--------|
| `auth` | `[jwt]` | Required first; needs `websocket.connect` + `devtools.console` |
| `devtools.stdin` | `[data]` | Raw terminal input string |
| `devtools.resize` | `[cols, rows]` | Decimal strings, e.g. `"120"`, `"40"` |

**Axis → Client**

| Event | Args |
|--------|------|
| `auth success` | `[]` |
| `devtools.status` | `[state]` |
| `devtools.stdout` | `[data]` |
| `devtools.error` | `[message]` |
| `token expiring` / `token expired` | same as console WS |
| `daemon error` | `[message]` |

On successful auth, Axis ensures the sidecar is running and attaches a `docker exec` TTY (`/bin/bash -l` or image `ENTRYPOINT` shell) in `/home/container`.

### Ephemeral SSH — `POST /api/servers/:server/devtools/ssh`

**Auth:** node token  

Creates (or rotates) a temporary SSH listener on the **node** with a random username/password. Auth success opens a docker-exec PTY into the sidecar.

**Port selection:**

- If Atlas sends a non-empty request pool (`ports` / `ssh_ports` / `port_pool` / `preferred_ports`) and/or `system.devtools.ports` is configured, Axis binds **only** from that pool (never a random outside port).
- Optional `port` is tried first when free; remaining pool ports are tried next (shuffled).
- If the pool is empty (request and config), Axis binds `ssh_bind:<random>` (legacy behavior).
- If every pool port is busy → HTTP error (pool exhausted); no silent fallback.
- Port `22` is never bound, even if listed.

Optional JSON body (empty body is fine — uses node config pool only):

```json
{
  "port": 40001,
  "ports": [40000, 40001, 40002],
  "ssh_ports": [40000, 40001, 40002],
  "port_pool": [40000, 40001, 40002],
  "preferred_ports": [40000, 40001, 40002]
}
```

**Response `200`:**

```json
{
  "contract": "devtools.v1",
  "mode": "ephemeral_ssh",
  "implemented": true,
  "port": 40001,
  "username": "dta1b2c3d4e5f678",
  "password": "…",
  "expires_at": "2026-09-05T12:00:00Z",
  "idle_timeout_seconds": 900,
  "session_ttl_seconds": 3600
}
```

Atlas should show **node FQDN/IP + port + username + password**. Session ends on idle (no SSH clients) or hard TTL. `DELETE /api/servers/:server/devtools/ssh` closes the listener early. Status `GET …/devtools` includes an `ssh` object while live. `GET …/devtools/ssh` (if used) binds from the node config pool only.

### Phase-2 bastion (optional later)

Central proxy at `dchu096.me` remains optional; per-node ephemeral SSH is the v1 path.
---

## Config surface (`config.yml`)

```yaml
system:
  devtools:
    # Master switch on the node. When false, all attach/enable calls fail closed.
    enabled: true
    # Sidecar image (admins pull/update via normal docker pull / image updates).
    image: ghcr.io/dchu096/atlas-devtools:latest
    # Memory limit in MiB
    memory: 512
    # CPU limit in percentage points (100 = 1 core), 0 = unlimited
    cpu: 100
    # Max PIDs inside sidecar
    pids_limit: 64
    # Stop container after this many seconds with no active PTY
    idle_timeout: 900
    # Hard lifetime for ephemeral SSH credentials (seconds)
    session_ttl: 3600
    # Address for ephemeral SSH listeners
    ssh_bind: "0.0.0.0"
    # Firewall-friendly SSH port pool. Non-empty = bind only from this list.
    # Empty = legacy random high port. Aliases: ssh_ports, port_pool. Never binds 22.
    ports: [40000, 40001, 40002]
    # tmpfs size for /tmp in MiB
    tmpfs_size: 64
    # Working directory inside sidecar
    workdir: /home/container
    # Shell command for PTY exec
    shell: ["/bin/bash", "-l"]
```

---

## Panel-facing fields (Atlas must store / sync)

Additive on server settings JSON synced to Axis (old panels omit → treated as disabled):

| Field | Type | Description |
|--------|------|-------------|
| `devtools_enabled` | `bool` | User/admin toggle for this server |
| `devtools_image` | `string` (optional) | Per-server image override; empty = node default |

Suggested Atlas permission: `devtools.console` (and panel UI gate).  
SFTP account SSH keys remain **SFTP-only** — do not reuse as interactive DevTools SSH auth without an explicit gateway design.

---

## Non-goals (v1)

- Full Teleport / public SSH gateway
- Guaranteeing malware cannot run in the **game** process after restart
- Replacing SFTP
- Secret-path hiding (`.env`, `*.pem`) — nice-to-have later

---

## Test plan

1. **Mount flags** — inspect sidecar mounts; data volume options include `noexec`; `/tmp` tmpfs includes `noexec`; attempt `./malware` from data dir fails; `git`/`curl` from image PATH work.
2. **Enable/disable** — POST enable → status armed; disable → container removed; attach fails with clear error.
3. **Attach auth** — JWT without `devtools.console` rejected; suspended server rejected.
4. **Delete** — delete server removes `_devtools` container; no docker.sock in mounts.
5. **Isolation** — cannot `docker`/`nsenter` host; CapDrop ALL; not privileged.
6. **Idle stop** — after timeout with no WS, container stops; re-attach starts again.
7. **Install interaction** — reinstall stops sidecar for the install window.

---

## Atlas handoff checklist

- [ ] Persist `devtools_enabled` on server
- [ ] Sync field in Wings/Axis server configuration payload
- [ ] Server → Terminal page (xterm) against `/devtools/ws`
- [ ] Permission `devtools.console`
- [ ] Deploy `axis-devtools` image to nodes / document pull
- [ ] Phase 2: bastion using identity `username.<serveruuid>`
