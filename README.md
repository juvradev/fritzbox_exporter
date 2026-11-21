# fritzbox_exporter

A Prometheus exporter for AVM FRITZ!Box routers, written in Go.

It reads data via the TR-064 API (and the web UI for DOCSIS values on cable models)
and exposes it on `/metrics`, ready to be scraped by Prometheus.

## Features

- **Internet throughput** – WAN byte counters (64 bit)
- **Connection drops** – connection state and uptime
- **WLAN clients** – connected clients per band (2.4 GHz, 5 GHz, guest)
- **Cable signal quality** – DOCSIS power level, MER (signal-to-noise) and error counters per channel
- Cloud-native: JSON logs, health endpoints, graceful shutdown, distroless non-root image (amd64 + arm64)

Tested with a **FRITZ!Box 6660 Cable** (FRITZ!OS 8.25).

## Prerequisites on the FRITZ!Box

1. **Enable TR-064:** *Home Network → Network → Network Settings → Home Network Sharing* →
   enable **"Allow access for applications"**.
2. **Create a dedicated user:** *System → FRITZ!Box Users → Add user*
   (e.g. `prometheus`) with the right **"FRITZ!Box settings"**.
   Do not use your admin account.

## Quick start (Docker / Podman)

```bash
docker run -d --name fritzbox_exporter \
  -p 9133:9133 \
  -e FRITZBOX_URL=http://192.168.178.1:49000 \
  -e FRITZBOX_USERNAME=prometheus \
  -e FRITZBOX_PASSWORD \
  ghcr.io/juvradev/fritzbox_exporter:latest
```

`-e FRITZBOX_PASSWORD` without a value takes the password from your shell, so it
does not end up in your shell history. Set it beforehand:

```bash
# bash
read -rsp "FRITZ!Box password: " FRITZBOX_PASSWORD && echo && export FRITZBOX_PASSWORD
# zsh
read -s "FRITZBOX_PASSWORD?FRITZ!Box password: " && export FRITZBOX_PASSWORD
```

Alternatively use an env file (`chmod 600`, no quotes around values):

```bash
docker run -d --name fritzbox_exporter -p 9133:9133 \
  --env-file ~/fritzbox.env \
  ghcr.io/juvradev/fritzbox_exporter:latest
```

Then check it:

```bash
curl -s localhost:9133/metrics | grep '^fritzbox_up'
# fritzbox_up 1
```

`podman` works the same way – just replace `docker` with `podman`.

### Docker Compose

```yaml
services:
  fritzbox-exporter:
    image: ghcr.io/juvradev/fritzbox_exporter:latest
    restart: unless-stopped
    ports:
      - "9133:9133"
    environment:
      FRITZBOX_URL: http://192.168.178.1:49000
      FRITZBOX_USERNAME: prometheus
      FRITZBOX_PASSWORD: ${FRITZBOX_PASSWORD} # from your shell or a .env file
      SCRAPE_TIMEOUT: 8s
```

## Configuration

All settings are environment variables.

| Variable              | Required | Default                    | Description |
|-----------------------|:--------:|----------------------------|-------------|
| `FRITZBOX_USERNAME`   | ✅       | –                          | FRITZ!Box user |
| `FRITZBOX_PASSWORD`   | ✅       | –                          | Password of that user |
| `FRITZBOX_URL`        |          | `http://fritz.box:49000`   | TR-064 URL of the box. Use the IP inside containers/Kubernetes, `fritz.box` is usually not resolvable there |
| `FRITZBOX_WEBUI_URL`  |          | derived from `FRITZBOX_URL` (same host, no port) | Web UI URL, only used by the `docsis` collector |
| `FRITZBOX_COLLECTORS` |          | `device,wan,wlan,docsis`   | Comma-separated list of enabled collectors |
| `LISTEN_ADDRESS`      |          | `:9133`                    | Address of the HTTP server |
| `LOG_LEVEL`           |          | `info`                     | `debug`, `info`, `warn` or `error` |
| `SCRAPE_TIMEOUT`      |          | `10s`                      | Max. duration of one scrape. Keep it below Prometheus' `scrape_timeout` (default 10s), e.g. `8s` |

The exporter exits immediately with a clear error if the configuration is invalid.
The password is never logged.

## Endpoints

| Path       | Purpose |
|------------|---------|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness: `200` while the process is running |
| `/readyz`  | Readiness: `200` while serving, `503` during shutdown |

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `fritzbox_up` | gauge | `1` if the box could be reached |
| `fritzbox_scrape_collector_success{collector}` | gauge | `1` if the collector succeeded |
| `fritzbox_scrape_collector_duration_seconds{collector}` | gauge | Duration of the collector |
| `fritzbox_info{model,firmware}` | gauge | Always `1` |
| `fritzbox_uptime_seconds` | gauge | Time since the last reboot |
| `fritzbox_wan_info{access_type}` | gauge | Always `1` |
| `fritzbox_wan_bytes_received_total` / `fritzbox_wan_bytes_sent_total` | counter | WAN traffic |
| `fritzbox_wan_max_bitrate_bits_per_second{direction}` | gauge | Max. bit rate of the link |
| `fritzbox_wan_physical_link_up` | gauge | Physical link state |
| `fritzbox_wan_connected` | gauge | Internet connection state |
| `fritzbox_wan_connection_uptime_seconds` | gauge | Time since the connection was established |
| `fritzbox_wlan_associations{index,band}` | gauge | Connected WLAN clients |
| `fritzbox_wlan_enabled{index,band}` | gauge | WLAN interface enabled |
| `fritzbox_wlan_info{index,band,ssid,standard,channel}` | gauge | Always `1` |
| `fritzbox_docsis_available` | gauge | `1` if the cable modem is in use |
| `fritzbox_docsis_power_level_dbuv{direction,docsis,channel}` | gauge | Power level per channel |
| `fritzbox_docsis_downstream_mer_db{docsis,channel}` | gauge | Signal-to-noise per downstream channel |
| `fritzbox_docsis_downstream_corrected_errors_total{docsis,channel}` | counter | Corrected codewords |
| `fritzbox_docsis_downstream_uncorrectable_errors_total{docsis,channel}` | counter | Lost codewords |
| `fritzbox_docsis_channel_info{direction,docsis,channel,frequency,modulation}` | gauge | Always `1` |

Plus the standard `go_*` and `process_*` metrics of the exporter itself.

### Useful PromQL

```promql
# Download / upload in Mbit/s
rate(fritzbox_wan_bytes_received_total[5m]) * 8 / 1e6
rate(fritzbox_wan_bytes_sent_total[5m]) * 8 / 1e6

# Reconnects in the last 24 hours
resets(fritzbox_wan_connection_uptime_seconds[24h])

# WLAN clients per band
sum by (band) (fritzbox_wlan_associations)

# Lowest downstream signal-to-noise
min(fritzbox_docsis_downstream_mer_db)
```

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: fritzbox
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
      - targets: ["<exporter-host>:9133"]
```

## Development

```bash
go test -race ./...          # unit tests
golangci-lint run            # linters

# run locally
FRITZBOX_URL=http://192.168.178.1:49000 FRITZBOX_USERNAME=prometheus go run .

# integration test against a real box (not run in CI)
FRITZBOX_URL=http://192.168.178.1:49000 FRITZBOX_USERNAME=prometheus \
  go test -tags integration -v ./internal/tr064

# build the image locally
docker build --build-arg VERSION=dev -t fritzbox_exporter:dev .
```

### Project layout

```
main.go               wiring: config → clients → collectors → HTTP server
internal/config       configuration from environment variables
internal/tr064        TR-064 SOAP client with HTTP digest auth
internal/webui        web UI client (PBKDF2 login, used for DOCSIS)
internal/collector    Prometheus collectors: device, wan, wlan, docsis
internal/server       HTTP server, health endpoints, graceful shutdown
```

## Releases

Images are built by GitHub Actions for `linux/amd64` and `linux/arm64`:

| Tag | Meaning |
|-----|---------|
| `latest`, `main` | Current state of the `main` branch |
| `X.Y.Z` | A release, created from a git tag `vX.Y.Z` – use this for deployments |
| `sha-<commit>` | A specific commit |