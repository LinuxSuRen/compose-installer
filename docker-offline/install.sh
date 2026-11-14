#!/usr/bin/env bash
# docker / docker-compose offline installer (Ubuntu x86_64/aarch64 + systemd).
# Shipped inside the docker-offline installer image under /offline; executed by
# compose-installer on the target host:
#   sudo bash install.sh --yes
# Traits: non-interactive, idempotent; skips when a healthy Docker exists
# (use --force to reinstall).
set -euo pipefail

log() { echo "[docker-offline] $*"; }
die() { echo "[docker-offline][ERROR] $*" >&2; exit 1; }

FORCE=0
for a in "$@"; do
  case "$a" in
    --force) FORCE=1 ;;
    --yes) ;;
    *) die "unknown argument: $a (supported: --yes / --force)" ;;
  esac
done

[[ ${EUID} -eq 0 ]] || die "please run as root (sudo bash install.sh --yes)"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---- platform checks ----
. /etc/os-release 2>/dev/null || die "cannot read /etc/os-release"
if [[ "${ID:-}" != "ubuntu" ]]; then
  log "warning: this system is ${ID:-unknown}; only Ubuntu is verified, continuing"
fi
# Architecture guard: materials are extracted by compose-installer for the
# target architecture from a multi-arch installer image; binaries must match
# the current machine to be allowed through.
case "$(uname -m)" in
  x86_64) ;;
  aarch64) ;;
  *) die "only x86_64/aarch64 supported (current: $(uname -m))" ;;
esac
command -v systemctl >/dev/null 2>&1 || die "systemctl not found (systemd required)"

# ---- skip when a healthy Docker already exists ----
if [[ ${FORCE} -eq 0 ]] && command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  log "a working Docker detected, skipping installation (use --force to reinstall)"
  docker compose version 2>/dev/null || docker-compose version >/dev/null 2>&1 || die "docker-compose missing, reinstall with --force"
  exit 0
fi

# ---- install binaries ----
log "installing docker binaries -> /usr/bin (materials: ${SRC})"
install -m 0755 "${SRC}"/docker/* /usr/bin/

log "installing docker-compose (/usr/bin and cli-plugin)"
install -D -m 0755 "${SRC}/bin/docker-compose" /usr/bin/docker-compose
mkdir -p /usr/lib/docker/cli-plugins
install -m 0755 "${SRC}/bin/docker-compose" /usr/lib/docker/cli-plugins/docker-compose

# ---- systemd services ----
log "installing systemd units and starting"
install -m 0644 "${SRC}"/systemd/docker.service "${SRC}"/systemd/docker.socket "${SRC}"/systemd/containerd.service /etc/systemd/system/
systemctl daemon-reload

getent group docker >/dev/null 2>&1 || groupadd --system docker
systemctl enable docker.service >/dev/null 2>&1 || true
# The binaries were just replaced by the materialized versions: a restart is
# required so the still-running old dockerd exits — enable --now only starts
# stopped services and never restarts an active one, otherwise Client (new
# binary) and Server (old process) end up version-skewed (observed: after
# installing 27.5.1 materials the Server kept reporting 29.7.1). Restarting
# affects running containers (the install flow rebuilds them anyway).
systemctl restart docker.service

# Under sudo, add the invoking user to the docker group so docker works
# without sudo after re-login.
if [[ -n "${SUDO_USER:-}" ]] && id "${SUDO_USER}" >/dev/null 2>&1; then
  usermod -aG docker "${SUDO_USER}" || true
  log "added ${SUDO_USER} to the docker group (effective after re-login)"
fi

# ---- verify (poll until the daemon is ready after restart, max 15s) ----
for _ in $(seq 1 15); do
  docker version >/dev/null 2>&1 && break
  sleep 1
done
docker version >/dev/null 2>&1 || die "docker verification failed, check: journalctl -u docker"
docker compose version >/dev/null 2>&1 || die "docker compose plugin verification failed"
srv="$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo ok)"
log "installation complete: docker ${srv}, $(docker compose version)"
# A mismatch between materialized and running daemon versions means the
# installation did not take effect (guards the Server=material regression).
if [[ "${srv}" != ok ]] && ! docker version --format '{{.Client.Version}}' 2>/dev/null | grep -q "^${srv}"; then
  die "Server(${srv}) and Client versions differ — the old dockerd was not replaced, run: systemctl restart docker"
fi
