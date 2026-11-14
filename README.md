<p align="center"><img src="docs/logo.svg" width="120" alt="compose-installer logo"></p>

# compose-installer

[![CI](https://github.com/LinuxSuRen/compose-installer/actions/workflows/ci.yml/badge.svg)](https://github.com/LinuxSuRen/compose-installer/actions/workflows/ci.yml)
[![Release](https://github.com/LinuxSuRen/compose-installer/actions/workflows/release.yml/badge.svg)](https://github.com/LinuxSuRen/compose-installer/actions/workflows/release.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Pack and install [docker compose](https://docs.docker.com/compose/) products — offline or online, from a browser or your phone.**

Shipping a compose-based product to a customer machine usually means a pile of
hand-written scripts: pull every image, `docker save`, copy configs, install
docker on an air-gapped Ubuntu box, generate `.env`, pray. compose-installer
turns that experience into two buttons:

- **Pack** (on an online machine with docker): resolves the full image list
  from the compose file, pulls per target architecture, saves everything with
  the project subset and docker/compose install materials into **one tar.gz**.
- **Install** (local or over SSH): uploads the bundle, installs docker when
  missing (offline materials or `get.docker.com`), loads images, assembles
  `.env`, brings the stack up — with live logs all the way.

Zero-config: point it at any compose project and pack. A small
[`compose-installer.json`](docs/config.md) unlocks multi-product /
multi-environment projects, version manifests, required env prompts and more.

Chinese documentation: [README.zh-CN.md](README.zh-CN.md)

## Highlights

Battle-tested details, not just plumbing:

- **Offline & online bundles**, one format — the installer auto-detects
  ([format spec](docs/bundle-format.md)). Online bundles are KB-sized; the
  target pulls images itself.
- **Cross-architecture safe**: multi-arch images are pulled **by platform
  digest** and re-tagged, so `docker save` exports exactly the target platform
  (avoids the containerd-store `content digest not found` trap); the installer
  compares `uname -m` and hard-fails on mismatches before wasting an upload.
- **Profile-trimmed bundles**: selected compose profiles are baked in and the
  `profiles:` markers stripped — self-contained bundles, no `--profile` on the
  target.
- **Pragmatic SSH installs**: key/agent/password auth chain, three-tier sudo
  escalation (root → `sudo -n` → `sudo -S` via stdin), scp→SFTP fallback,
  md5-based incremental upload for GB bundles, sftp subsystem auto-enable.
- **Host conditioning**: docker ≥ 20.10 enforced on cgroup v2 hosts; NAT fix
  for `daemon.json "iptables": false`; inotify/sysctl quotas raised
  idempotently (raise-only) before `compose up`; disk-space checks.
- **Fail-closed `.env`**: template + version-manifest tags + computed defaults
  + user overrides, with `requiredEnv` keys enforced on both UI and backend —
  no silently inherited dev defaults.
- **Web UI + REST API**: the same API powers the mobile clients
  ([Android](https://github.com/LinuxSuRen/compose-installer-android),
  [HarmonyOS](https://github.com/LinuxSuRen/compose-installer-harmony)) —
  see [docs/api.md](docs/api.md).
- **`doctor` subcommand**: standalone diagnosis of container-network (NAT),
  inotify quota, kernel limits and docker/cgroup mismatches, with copy-paste
  fixes.

## Install

Three paths, same version:

```bash
# 1. go install (Go >= 1.25)
go install github.com/LinuxSuRen/compose-installer@latest

# 2. prebuilt binaries from GitHub Releases
#    https://github.com/LinuxSuRen/compose-installer/releases

# 3. container image (multi-arch, delivery form — Pack tab hidden)
docker run --rm -p 8765:8765 ghcr.io/linuxsuren/compose-installer
```

## Quick start

```bash
compose-installer
# compose-installer v0.1.0 started: http://127.0.0.1:8765 (browser opens)
```

1. **Pack page** — pick your compose project directory, product/env/version,
   target architecture; optionally provide a
   [docker-offline installer image](docker-offline/) so the bundle can
   bootstrap docker itself; press **Start**. The bundle lands in `<project>/dist/`.
2. **Install page** — pick the bundle (the manifest summary and required env
   fields appear automatically), choose local or SSH, fill in the required env
   values, press **Start** and watch the logs.

Try the bundled [`example/`](example/) project first — it demonstrates version
manifests, `requiredEnv` and `envRewrites` in ~20 lines.

Flags: `-addr` (default `127.0.0.1:8765`, auto-falls-forward when busy),
`-open-browser=false`, `-hide-pack-tab` (delivery form),
`-v`. Diagnose a docker host:

```bash
compose-installer doctor   # exit 0 healthy / 1 issues found / 2 cannot diagnose
```

## Configuring your project

Without configuration, any compose project works (auto-detected compose file,
single `prod` environment). Add [`compose-installer.json`](docs/config.md) for:

```json
{
  "defaultProduct": "platform",
  "products": {
    "platform": {
      "composeFiles": {"prod": "docker-compose.yml", "test": "docker-compose.test.yml"},
      "versions": {"prod": "versions/prod.json"},
      "requiredEnv": ["JWT_SECRET"],
      "envRewrites": {"UPLOADS_HOST_PATH": "{installDir}/data/uploads"}
    }
  }
}
```

Version manifests map a major version to per-component image tags — partial
releases only bump what changed:

```json
{"latest": "1.0", "versions": {"1.0": {"backend": "0.0.23", "frontend": "0.0.23"}}}
```

## Docker offline materials

`docker-offline/` builds the companion image carrying docker/compose binaries
and systemd units, extracted into offline bundles:

```bash
cd docker-offline
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/linuxsuren/compose-installer/docker-offline:27.5.1 --push .
```

## Documentation

- [Project configuration](docs/config.md)
- [Bundle format](docs/bundle-format.md)
- [REST API (mobile clients)](docs/api.md)
- [Example project](example/)

## Contributing

Issues and pull requests are welcome. Open an issue first for larger changes;
keep PRs focused on one topic. See [LICENSE](LICENSE) (Apache-2.0).

## Origin

Distilled from a production robot-platform deployment tool that has been
packing and installing multi-arch, offline-first compose products in the field
— the hard-won behaviors (digest pulls, sudo chains, NAT fixes, quota tuning)
are preserved, the product-specific parts are now configuration.
