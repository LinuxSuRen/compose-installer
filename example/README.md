# Demo project

A minimal compose project showing every configuration feature on one page:

- one product (`demo`), one environment (`prod`) with a version manifest
  (`versions/prod.json` → `NGINX_TAG` / `HTTP_ECHO_TAG`)
- `requiredEnv`: `DEMO_API_TOKEN` must be filled on the Install page
- `envRewrites`: `DEMO_DATA_DIR` defaults to `{installDir}/data`
- `envExports`: `APP_VERSION` is derived from the selected version
- `./site` is collected automatically (compose local-mount scan)

## Try it

```bash
# from the compose-installer repo root
compose-installer -addr 127.0.0.1:8765
```

1. Pack page → project dir = this `example/` directory → product `demo`,
   env `prod`, version `1.0` → **Start** (offline bundle pulls nginx +
   http-echo; check "Online bundle" to skip images)
2. Install page → pick the produced `dist/demo-prod-1.0-…tar.gz`, fill
   `DEMO_API_TOKEN`, choose local or SSH → **Start**
3. Visit `http://<target>:8080`
