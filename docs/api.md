# REST API

The web UI and the mobile clients ([Android](https://github.com/LinuxSuRen/compose-installer-android),
[HarmonyOS](https://github.com/LinuxSuRen/compose-installer-harmony)) talk to the same
HTTP API served by the `compose-installer` binary. All endpoints are JSON; errors
use `{"error": "message"}` with an appropriate HTTP status code.

Long-running work (pack, install) is asynchronous: `POST` returns a `jobId`
immediately, the client polls `GET /api/jobs/{id}?offset=N` and renders the
incremental log lines.

## Health & host

### `GET /api/health`

```json
{"status": "ok", "version": "v0.1.0", "hidePack": "false"}
```

`hidePack` reflects the `-hide-pack-tab` flag (delivery/container form hides the
Pack page).

### `GET /api/host-info`

Describes the machine running the tool (the Install page adapts to it):

```json
{"os": "windows", "osLabel": "Windows", "isRoot": false, "bash": true, "home": "C:\\Users\\rick"}
```

## Project inspection (Pack page)

### `GET /api/project?projectDir=<path>`

`projectDir` empty = auto-detect upward from the executable. Response:

```json
{
  "projectDir": "/srv/myapp",
  "defaultProduct": "platform",
  "products": {
    "platform": {
      "installDirName": "platform-deploy",
      "envs": ["prod", "test"],
      "versions": {"test": ["1.0", "1.1"]},
      "latest": {"test": "1.1"},
      "profiles": {"test": ["gpu", "edge"]},
      "error": null
    }
  }
}
```

- `versions`/`latest` are `null` for environments without a version manifest —
  the client hides the version picker for those.
- `profiles` are discovered from the environment's compose file.

### `GET /api/fs?path=<path>`

Directory listing for path pickers: `{"path": "...", "dirs": [...], "files": [...]}`.

## Bundle inspection (Install page)

### `GET /api/bundle-manifest?path=<bundle.tar.gz>`

Returns the bundle manifest (see [bundle-format.md](bundle-format.md)). The
client shows the summary card and builds the required-env form from
`requiredEnv`.

### `GET /api/default-install-dir?name=<dirName>&mode=<local|remote>&user=<sshUser>`

```json
{"dir": "/opt/myapp"}
```

## Jobs

### `POST /api/pack`

Body: [`PackRequest`](#packrequest). Response: `{"jobId": "pack-1731584400-1"}`.

#### PackRequest

```json
{
  "projectDir": "/srv/myapp",
  "product": "platform",
  "env": "test",
  "version": "1.0",
  "profiles": ["gpu"],
  "installerImage": "ghcr.io/linuxsuren/compose-installer/docker-offline:27.5.1",
  "installerPath": "/offline",
  "output": "",
  "registry": "",
  "registryUser": "",
  "registryPassword": "",
  "online": false,
  "platform": "linux/arm64"
}
```

| Field | Notes |
|---|---|
| `product`/`env` | select from `GET /api/project` |
| `version` | major version; empty = `latest`; ignored for unversioned environments |
| `profiles` | compose profiles baked into the bundle |
| `installerImage` | image carrying docker/compose offline materials under `installerPath` (needs `install.sh`); offline bundles only |
| `online` | online bundle: no images, no docker materials |
| `platform` | `linux/amd64` / `linux/arm64`; empty or `auto` = packing machine |

### `POST /api/install`

Body: [`InstallRequest`](#installrequest). Response: `{"jobId": "install-…"}`.

#### InstallRequest

```json
{
  "mode": "remote",
  "ssh": {
    "host": "192.168.31.10",
    "port": 22,
    "user": "ubuntu",
    "password": "",
    "keyFile": "",
    "keyPassphrase": ""
  },
  "bundlePath": "C:\\dist\\myapp-test-1.0-linux-amd64.tar.gz",
  "installDir": "",
  "dockerInstall": "auto",
  "dockerForce": false,
  "installScript": "",
  "installScriptArgs": [],
  "envVars": {"DEVICE_ID": "robot-42"},
  "sudoPassword": "",
  "registry": "",
  "registryUser": "",
  "registryPassword": "",
  "natFixPolicy": "restart"
}
```

| Field | Notes |
|---|---|
| `mode` | `local` or `remote` (SSH) |
| `installDir` | empty = default (`/opt/<dirName>` for root, home dir otherwise) |
| `dockerInstall` | `auto` \| `offline` \| `skip` |
| `envVars` | `.env` overrides (highest precedence); must cover every `requiredEnv` key of the manifest |
| `natFixPolicy` | remedy for `daemon.json "iptables": false`: `restart` \| `manual` \| `skip` |
| `registry*` | online bundles: docker login on the target before `docker compose pull`; the login state persists in the target's docker config (same as a manual `docker login`) so re-installs can leave it empty |

Secrets (`password`, `keyPass`, `sudoPassword`, `registryPassword`) are passed
through stdin/SSH channels only, never persisted by the tool.

### `GET /api/jobs/{id}?offset=N`

```json
{
  "id": "pack-1731584400-1",
  "kind": "pack",
  "status": "running",
  "err": "",
  "lines": ["[18:04:01] target: platform / env: test"],
  "total": 42,
  "startedAtMs": 1731584401000,
  "finishedAtMs": 1731584521000,
  "durationMs": 120000
}
```

- `status`: `running` \| `success` \| `failed` (`err` holds the reason on failure)
- `lines` are the log lines after `offset`; `total` is the new offset
- poll every 1–2 s while `status == "running"`

## Suggested mobile flow

1. `GET /api/health` — connectivity + version
2. User picks a bundle file (already transferred to the machine running
   `compose-installer`, or uploaded out-of-band) → `GET /api/bundle-manifest`
3. Render the required-env form from `requiredEnv`
4. `POST /api/install` → poll `GET /api/jobs/{id}` until terminal status
