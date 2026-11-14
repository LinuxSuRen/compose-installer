# Bundle format

A bundle is a **single tar.gz** produced by the Pack step and consumed by the
Install step (web UI or mobile clients). The filename carries human-readable
metadata:

```
<product>-<env>-<version>[-<profile1>+<profile2>…][-online]-<arch>-<timestamp>.tar.gz
myapp-test-1.0-gpu+edge-linux-arm64-20260101120000.tar.gz
```

## Layout

```
manifest.json          the manifest — the only contract between packer & installer
repo/                  the compose project subset
  <workdir>/           compose file renamed to docker-compose.yml, .env
                       template, extra files, referenced config dirs…
images/<product>.tar   docker save of every image (offline bundles only)
docker-offline/        docker/compose install materials extracted from the
                       installer image, incl. install.sh (offline bundles only)
```

- The selected environment's compose file is **renamed to
  `docker-compose.yml`** inside the bundle: the target machine never deals with
  environment suffixes and other environments' topologies are not leaked.
- When profiles are selected the compose is **trimmed** to those services and
  the `profiles:` markers are stripped — the bundle is self-contained, the
  installer passes no `--profile`. Services without profiles are always kept
  (compose semantics).
- The repo subset is assembled from a whitelist (compose, env template, version
  manifest, extra files) **plus every `./dir` local mount referenced by the
  compose file** — missing bind sources would be silently auto-created as
  directories by dockerd and break services. Known blind spot: only the short
  `volumes` syntax is scanned.
- Runtime dirs (`data/`, `certs/`, host metadata) never enter the bundle.

## manifest.json

Fields are **additive only** — old tools must read new bundles. Current schema
(`schemaVersion: 1`):

| Field | Type | Notes |
|---|---|---|
| `schemaVersion` | int | `1` |
| `created` | time | pack time |
| `product` | string | product name from compose-installer.json (`"default"` for zero-config projects) |
| `env` | string | selected environment |
| `version` | string | major version label (may be empty) |
| `composeFile` | string | always `docker-compose.yml` in schema 1 |
| `workdir` | string | compose dir relative to `repo/` (`.` or a subdirectory) |
| `profiles` | []string | empty once trimmed |
| `images` | []string | every image reference (offline: saved; online: pulled by target) |
| `components` | map | component → tag for versioned products |
| `online` | bool | online bundle: no `images/`, no `docker-offline/` |
| `platform` | string | `linux/amd64` \| `linux/arm64`; offline install aborts on mismatch with the target, online only warns |
| `installDirName` | string | default install dir name (`/opt/<name>` for root targets) |
| `requiredEnv` | []string | env keys the installer must collect from the user |
| `envExports` | map | extra `.env` entries; `{version}` placeholder |
| `envRewrites` | map | install-time `.env` defaults; `{installDir}` placeholder; user overrides win |

## Online vs offline

| | Offline | Online |
|---|---|---|
| Size | GB-scale (images included) | KB–MB |
| Images | `docker load` from `images/<product>.tar` | `docker login` (stdin creds) + `docker compose pull` with retries on the target |
| Docker install | bundle materials (`docker-offline/install.sh`) | `get.docker.com` script when missing (Linux only) |

The installer branches automatically on `online` — users never choose a mode.

## Target `.env` assembly order

```
<env template>  →  <COMPONENT>_TAG=<tag> per manifest.components
               →  envExports ({version} expanded)
               →  envRewrites ({installDir} expanded; skipped for user-provided keys)
               →  user envVars (highest precedence)
```

Every `requiredEnv` key must be non-empty in the final result — the installer
rejects the request otherwise, before connecting to the target.
