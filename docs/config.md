# Project configuration (compose-installer.json)

`compose-installer.json` is an **optional** file at the root of a compose
project. It tells the packer which products the project ships, which compose
file belongs to which environment, where version manifests live, and which env
keys the installer must collect from the user.

**Without it**, compose-installer runs in zero-config mode: a single product
(`default`) with one environment (`prod`) pointing at the auto-detected compose
file (`docker-compose.yml`, `docker-compose.yaml`, `compose.yaml`, `compose.yml`).

## Full example

```json
{
  "defaultProduct": "platform",
  "products": {
    "platform": {
      "workdir": ".",
      "composeFiles": {
        "prod": "docker-compose.yml",
        "test": "docker-compose.test.yml",
        "dev": "docker-compose.dev.yml"
      },
      "envTemplate": ".env.example",
      "versions": {
        "test": "versions/test.json",
        "prod": "versions/prod.json"
      },
      "envExports": {"APP_VERSION": "{version}"},
      "extraFiles": ["ops/"],
      "defaultProfiles": [],
      "requiredEnv": ["JWT_SECRET"],
      "envRewrites": {"UPLOADS_HOST_PATH": "{installDir}/data/uploads"},
      "installDirName": "platform-deploy"
    },
    "robot": {
      "workdir": "robot",
      "composeFiles": {
        "prod": "docker-compose.yml",
        "test": "docker-compose.test.yml",
        "dev": "docker-compose.dev.yml"
      },
      "versions": {
        "test": "robot/versions/test.json",
        "prod": "robot/versions/prod.json"
      },
      "defaultProfiles": ["mj"],
      "requiredEnv": ["DEVICE_ID", "INSPECTION_AUTH_TOKEN", "MQTT_BROKER"],
      "installDirName": "robot"
    }
  }
}
```

## Field reference

Paths are relative to the project root, except `composeFiles` and
`envTemplate`, which are relative to `workdir`.

| Field | Default | Description |
|---|---|---|
| `workdir` | `"."` | directory holding the compose file |
| `composeFiles` | — | **required** (zero-config aside): environment → compose file. The selected file is renamed to `docker-compose.yml` in the bundle |
| `envTemplate` | `".env.example"` | `.env` template shipped in the bundle; missing file is not an error |
| `versions` | — | environment → version manifest path (schema below). Environments without an entry use the tags baked into the compose/env template |
| `envExports` | — | extra `.env` entries written at install time; values support `{version}` |
| `extraFiles` | — | additional files/dirs to ship (relative to `workdir`) |
| `defaultProfiles` | — | preselected compose profiles |
| `requiredEnv` | — | env keys the installer must collect (UI + backend enforced) |
| `envRewrites` | — | install-time `.env` defaults, `{installDir}` placeholder; user overrides win |
| `installDirName` | product name | last segment of the default install dir (`/opt/<name>` for root, `~/<name>` otherwise) |

## Version manifest schema

One file per environment (`versions/<env>.json`): a **major version** maps to
per-component image tags. Partial releases bump only the components they touch;
`latest` is the recommended default.

```json
{
  "latest": "1.0",
  "versions": {
    "1.0": {
      "frontend": "0.0.23-rc",
      "backend": "0.0.23-rc",
      "detection-server": "0.0.14-rc"
    }
  }
}
```

At pack time each component `foo` becomes `FOO_TAG=<tag>` in the environment of
`docker compose config` (resolving `image: .../foo:${FOO_TAG:?…}` references),
and the same mapping is written to the bundle manifest so the installer
reproduces the identical `.env` on the target.

## Why `requiredEnv` + `envRewrites`

Compose files often carry development defaults (broker addresses, secrets).
Shipping those silently to production is a classic footgun, so:

- `requiredEnv` forces explicit input — no silently inherited defaults;
- `envRewrites` relocates state into the install directory
  (`{installDir}/data/...`) making installs self-contained, unless the user
  overrides.
