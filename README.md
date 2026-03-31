# unifi-hass-webhook

Small Go webhook service that verifies UniFi Access webhook signatures, filters door unlock events, and triggers a Home Assistant script.

## What it does

1. Receives `POST /unifi/webhook`.
2. Verifies UniFi `Signature` header (`t=<unix>,v1=<hex-hmac>`).
3. Enforces security checks:
   - max body size: `1 MiB`
   - timestamp skew: `±30s`
   - replay protection: `5m` TTL cache
4. Filters to allowed events only:
   - `event == access.door.unlock`
   - `data.object.result == Access Granted`
   - allowlist checks for policy, actor, and device
5. Calls Home Assistant:
   - `POST /api/services/script/turn_on`
   - sends your script `entity_id` and UniFi event data in `variables`

## Configuration

Environment variables:

| Variable                     | Required | Default | Description                                                             |
| ---------------------------- | -------- | ------- | ----------------------------------------------------------------------- |
| `LISTEN_ADDRESS`             | No       | `:8080` | HTTP listen address                                                     |
| `UNIFI_WEBHOOK_SECRET`       | Yes      | -       | Shared HMAC secret for UniFi webhook signatures                         |
| `UNIFI_ALLOWED_POLICY_IDS`   | Yes      | -       | Comma-separated allowed policy IDs                                      |
| `UNIFI_ALLOWED_ACTOR_IDS`    | Yes      | -       | Comma-separated allowed actor IDs                                       |
| `UNIFI_ALLOWED_DEVICE_IDS`   | Yes      | -       | Comma-separated allowed device IDs                                      |
| `HA_BASE_URL`                | Yes      | -       | Home Assistant base URL (for example `http://homeassistant.local:8123`) |
| `HA_TOKEN`                   | Yes      | -       | Long-lived Home Assistant token                                         |
| `HA_SCRIPT_ENTITY_ID`        | Yes      | -       | Script entity to call (for example `script.unifi_access_granted`)       |

Example values:

```env
LISTEN_ADDRESS=:8000

UNIFI_WEBHOOK_SECRET=replace_me
UNIFI_ALLOWED_POLICY_IDS=<policy-id-from-step-3> # Policy IDs from your Access deployment (comma seperated)
UNIFI_ALLOWED_ACTOR_IDS=<actor-id-1>,<actor-id-2> # User IDs from your Access deployment (comma seperated)
UNIFI_ALLOWED_DEVICE_IDS=<device-id> # Access Hub IDs from your Access deployment (comma seperated)

HA_BASE_URL=https://home-assistant.example.com
HA_TOKEN=replace_me
HA_SCRIPT_ENTITY_ID=script.retract_apartment_door_latch
```

## Container configuration

The service already reads configuration directly from process environment variables. You do not need an `.env` file in production.

That means on MikroTik RouterOS / The Dude / Rose container UI you can define the variables in the container settings screen and run the published image directly.

Required runtime variables:

```text
UNIFI_WEBHOOK_SECRET
UNIFI_ALLOWED_POLICY_IDS
UNIFI_ALLOWED_ACTOR_IDS
UNIFI_ALLOWED_DEVICE_IDS
HA_BASE_URL
HA_TOKEN
HA_SCRIPT_ENTITY_ID
```

Optional runtime variables:

```text
LISTEN_ADDRESS=:8080
```

## Run with Docker Compose

```bash
docker compose up -d
```

Service name: `unifi-hass-webhook`  
Container listens on `LISTEN_ADDRESS` and uses host networking (`network_mode: host`).

`docker-compose.yml` now expects values to be supplied as environment variables by your shell, CI/CD system, or container UI. It does not rely on `env_file`.

To use the published image, set:

```bash
IMAGE_NAME=ghcr.io/<your-github-user-or-org>/unifi-hass-webhook:latest
```

## GitHub Actions image publishing

The repository includes a workflow at `.github/workflows/docker-publish.yml` that builds and publishes a multi-arch image to GitHub Container Registry (`ghcr.io`).

What it does:

- builds for `linux/amd64` and `linux/arm64`
- pushes on every commit to `main`
- pushes version tags such as `v1.0.0`
- also supports manual runs from the Actions tab

Published image naming:

```text
ghcr.io/<owner>/<repo>:latest
ghcr.io/<owner>/<repo>:main
ghcr.io/<owner>/<repo>:<git-tag>
ghcr.io/<owner>/<repo>:sha-<commit>
```

Before pulling from RouterOS:

1. Push this repository to GitHub.
2. Enable GitHub Actions for the repo if needed.
3. Let the workflow publish the package.
4. If your package is private, create a GitHub personal access token with package read access and use that in RouterOS.
5. If you want anonymous pulls on the MikroTik, make the GHCR package public.

## MikroTik Rose / RouterOS container example

Use the image:

```text
ghcr.io/<your-github-user-or-org>/unifi-hass-webhook:latest
```

Pass the same environment variable names from the RouterOS container UI. Example values:

```text
LISTEN_ADDRESS=:8080
UNIFI_WEBHOOK_SECRET=replace_me
UNIFI_ALLOWED_POLICY_IDS=<policy-id>
UNIFI_ALLOWED_ACTOR_IDS=<actor-id-1>,<actor-id-2>
UNIFI_ALLOWED_DEVICE_IDS=<device-id>
HA_BASE_URL=https://home-assistant.example.com
HA_TOKEN=replace_me
HA_SCRIPT_ENTITY_ID=script.retract_apartment_door_latch
```

## Run locally

From the repository root:

```bash
cd src
go mod download
go run .
```

## Home Assistant payload

The service calls `script.turn_on` and passes:

- `entity_id`
- `variables.unifi_event_name`
- `variables.unifi_event_object_id`
- `variables.unifi_signature_time`
- `variables.unifi_received_at`
- `variables.unifi_event` (parsed object)
- `variables.unifi_event_json` (raw JSON string)

## UniFi webhook setup

Configure UniFi Access webhook URL to:

```text
http://<host>:8080/unifi/webhook
```

Use the same shared secret in UniFi and `UNIFI_WEBHOOK_SECRET`.

## Response codes

- `200 OK`: accepted and forwarded to Home Assistant
- `202 Accepted`: valid request but filtered out (not allowed or replay)
- `400 Bad Request`: malformed body or invalid JSON
- `401 Unauthorized`: invalid/missing signature or stale timestamp
- `413 Payload Too Large`: request body exceeds limit
- `502 Bad Gateway`: Home Assistant call failed

## Security notes

- Keep `UNIFI_WEBHOOK_SECRET` and `HA_TOKEN` private.
- Use a dedicated HA token.
- Prefer running behind trusted local networking or a reverse proxy with TLS.
