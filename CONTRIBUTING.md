# Contributing

Thanks for your interest in improving compose-installer!

## Getting started

- **Issues**: search existing issues before opening a new one; include full
  context (environment, versions, steps, expected vs actual, key error text —
  screenshots never replace text).
- **Pull requests**: one topic per PR, developed on a feature branch (never
  commit to master directly). For larger changes, open an issue first and
  sketch the plan.

## Development

```bash
make web          # build the Vue UI into web/dist (embedded by the Go binary)
make build-local  # build the binary for your platform
make test         # go test ./...
make lint         # go vet
```

Requirements: Go >= 1.25, Node.js + pnpm for the web UI. The web UI must be
built (or created via `make clean`) before `go build` — the binary embeds
`web/dist`.

Layout:

| Path | Content |
|---|---|
| `main.go` | HTTP server, REST API, embedded SPA |
| `pack.go` / `repo.go` | pack engine + version manifests |
| `install.go` | install engine (local/SSH, docker bootstrap, `.env`, host conditioning) |
| `doctor.go` | standalone host diagnostics |
| `runner.go` / `jobs.go` / `fs.go` | execution & job infrastructure |
| `config.go` / `manifest.go` / `types.go` | contracts: project config, bundle manifest, API types |
| `web/` | Vue 3 UI |
| `docker-offline/` | companion image carrying docker/compose install materials |
| `example/` | demo compose project exercising all config features |

Conventions:

- Keep the REST API and bundle manifest **additive-only** (see
  [docs/api.md](docs/api.md), [docs/bundle-format.md)) — mobile clients depend
  on them.
- The install/pack engines encode years of field experience (digest pulls,
  sudo chains, NAT fixes, quota tuning). Preserve behavior; document *why*
  in comments when touching it.
- Run `make lint test` before submitting; CI must stay green.

## Releasing

Maintainers: tags (`vX.Y.Z`) trigger [Release](.github/workflows/release.yml)
— goreleaser binaries + multi-arch container image from the same commit.
