# ecoflow-mqtt-presence

Small helper service that keeps an active MQTT session in EcoFlow Cloud so REST metrics stay fresh.

This is useful when EcoFlow starts updating cloud metrics too rarely unless there is an active client session, for example the mobile app. The service connects to EcoFlow MQTT with credentials from Open API and keeps the session alive. It can also monitor Smart Plug heartbeat values and recreate the MQTT session when presence appears to degrade.

Current version: `0.3.0`

## What it does

- requests MQTT credentials from EcoFlow Open API
- connects to EcoFlow MQTT over TLS
- subscribes to:
  - `/open/<user>/+/quota`
  - `/open/<user>/+/status`
  - `/open/<user>/+/set_reply`
- optionally polls all Smart Plugs on the account and checks `2_1.heartbeatFrequency`
- recreates the MQTT session if any Smart Plug heartbeat reaches the configured threshold

## Configuration

Required:

- `ECOFLOW_API_HOST`
- `ECOFLOW_ACCESS_KEY`
- `ECOFLOW_SECRET_KEY`

Optional:

- `ECOFLOW_CLIENT_ID` default: `ecoflow_presence_static`
- `ECOFLOW_KEEPALIVE` default: `30`
- `ECOFLOW_QOS` default: `0`
- `ECOFLOW_QUIET` default: `false`
- `ECOFLOW_HEALTHCHECK_TYPE` default: `false`
  - supported values: `false`, `smartplug`
- `ECOFLOW_HEALTHCHECK_INTERVAL` default: `60`
  - polling interval in seconds
- `ECOFLOW_HEALTHCHECK_SMARTPLUG_MAX_HEARTBEAT` default: `900`
  - if any Smart Plug reports `2_1.heartbeatFrequency >= threshold`, MQTT session is recreated

Example `.env`:

```env
ECOFLOW_API_HOST=api-e.ecoflow.com
ECOFLOW_ACCESS_KEY=YOUR_ACCESS_KEY
ECOFLOW_SECRET_KEY=YOUR_SECRET_KEY
ECOFLOW_CLIENT_ID=ecoflow_presence_static
ECOFLOW_KEEPALIVE=30
ECOFLOW_QOS=0
ECOFLOW_QUIET=false
ECOFLOW_HEALTHCHECK_TYPE=smartplug
ECOFLOW_HEALTHCHECK_INTERVAL=60
ECOFLOW_HEALTHCHECK_SMARTPLUG_MAX_HEARTBEAT=900
```

## Run with Docker

```bash
docker run --rm \
  --name ecoflow-presence \
  --env-file .env \
  nekoyos/ecoflow-presence:0.3.0
```

Persistent run:

```bash
docker run -d \
  --name ecoflow-presence \
  --restart unless-stopped \
  --env-file .env \
  nekoyos/ecoflow-presence:0.3.0
```

## Run with Docker Compose

```bash
docker compose up -d
```

The provided `docker-compose.yml` uses `nekoyos/ecoflow-presence:latest`.

## Build locally

Binary:

```bash
go build -o ecoflow .
./ecoflow --version
```

Docker image:

```bash
docker build -t ecoflow-presence:0.3.0 .
```

## Logs

The service logs:

- MQTT credential refresh and session creation
- subscription state
- every Smart Plug healthcheck cycle
- `2_1.heartbeatFrequency` value for each Smart Plug
- reconnect reason when the session is recreated

## Notes

- Smart Plug healthcheck is based on `2_1.heartbeatFrequency`, not `2_1.freq`
- `2_1.freq` is mains frequency, typically `50`, and is not suitable for presence detection
- if `ECOFLOW_QUIET=false`, incoming MQTT payloads are printed to stdout
