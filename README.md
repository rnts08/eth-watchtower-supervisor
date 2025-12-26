# ETH Watchtower Supervisor

The **ETH Watchtower Supervisor** is a robust process manager designed to ensure the `eth-watchtower` daemon (or any specified command) remains running, healthy, and observable. It handles background execution, automatic restarts, log management, and provides HTTP endpoints for monitoring and control.

## Features

*   **Detached by Default:** Automatically spawns into the background unless specified otherwise.
*   **Automatic Restarts:** Monitors the child process and restarts it if it crashes or exits.
*   **Health Monitoring:** Provides an HTTP API to check the status of the supervised process.
*   **Prometheus Metrics:** Exposes restart counts, uptime, and status metrics.
*   **Log Management:** Captures `stdout` and `stderr` to a file, with support for rotation and retention policies.
*   **Rich Dashboard:** Web interface with real-time resource usage charts, log viewer (with filtering), and dark mode.
*   **Dynamic Configuration:** Reloads configuration and rotates logs via HTTP or `SIGHUP` without stopping the supervisor.
*   **Timeout Protection:** Enforces a maximum execution time for the child process if configured.

## Building

Ensure you have Go installed.

```bash
go build -o eth-watchtower-supervisor main.go
```

## Configuration

The supervisor uses a JSON configuration file (default: `config.json`). It shares the configuration structure with the main watcher app but specifically looks for the `watcher` and `monitor` sections.

### Example `config.json`

```json
{
  "watcher": {
    "daemon": "./eth-watchtower -config config.json",
    "interval": 5,
    "log": "eth-watch-monitor.log",
    "metrics_enabled": true,
    "log_retention_days": 7,
    "dashboard_password": "admin"
  },
  "monitor": {
    "dashboard_enabled": true,
    "process_timeout_seconds": 300
  }
}
```

### Configuration Fields

| Section | Field | Description |
| :--- | :--- | :--- |
| `watcher` | `daemon` | The command string to run the child process. |
| `watcher` | `interval` | Delay (in seconds) before restarting the daemon after an exit. |
| `watcher` | `log` | Path to the log file where supervisor and daemon output is written. |
| `watcher` | `metrics_enabled` | If `true`, enables the `/metrics` Prometheus endpoint. |
| `watcher` | `log_retention_days` | Number of days to keep rotated log files. Set to `0` to disable cleanup. |
| `watcher` | `dashboard_password` | Optional password for Basic Auth protection on the dashboard and control endpoints. |
| `monitor` | `dashboard_enabled` | If `true`, enables the `/dashboard` HTML interface. |
| `monitor` | `process_timeout_seconds` | Maximum duration the daemon is allowed to run before being killed and restarted. |

## Usage

Start the supervisor:

```bash
./eth-watchtower-supervisor
```

By default, it will detach and run in the background.

### Command Line Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| `-config` | `config.json` | Path to the configuration file. |
| `-foreground` | `false` | Run in the foreground (do not detach). Useful for debugging or systemd services. |
| `-health-addr` | `:8080` | Address and port for the HTTP health/metrics server. |

## API Endpoints

The supervisor exposes an HTTP server (default port `8080`) for monitoring and control.
If `dashboard_password` is set, sensitive endpoints require HTTP Basic Authentication.

### `GET /dashboard`
A rich HTML dashboard to visualize status, view charts, filter logs, and control the supervisor.

### `GET /health`
Returns the current status of the supervised process.

**Response:**
```json
{
  "running": true,
  "pid": 12345,
  "start_time": "2023-10-27T10:00:00Z",
  "last_error": "",
  "restarts": 0,
  "cpu_percent": 1.2,
  "memory_bytes": 15728640
}
```

### `GET /metrics`
Returns Prometheus metrics (if `metrics_enabled` is true).

*   `eth_watchtower_daemon_restarts_total`: Counter of total restarts.
*   `eth_watchtower_daemon_start_timestamp_seconds`: Unix timestamp of the last start.
*   `eth_watchtower_daemon_up`: Gauge (1 = running, 0 = stopped).

### `POST /restart`
Manually triggers a restart of the child process.

```bash
curl -X POST http://localhost:8080/restart
```

### `POST /reload`
Reloads the configuration file from disk without restarting the supervisor itself.

```bash
curl -X POST http://localhost:8080/reload
```

## Signal Handling

The supervisor responds to OS signals for management:

*   **`SIGHUP`**: Reloads the configuration file and rotates the log file.
    *   The current log is renamed with a timestamp (e.g., `logfile.log.20231027-100000`).
    *   Old logs exceeding `log_retention_days` are deleted.
*   **`SIGINT` / `SIGTERM`**: Gracefully shuts down the supervisor and the child process.

## Log Rotation

Logs can be rotated manually via `SIGHUP` or by using an external tool like `logrotate` that sends the signal.

To trigger rotation manually:
```bash
pkill -HUP eth-watchtower-supervisor
```

## Systemd Integration

If running via `systemd`, use the `-foreground` flag so systemd can manage the process lifecycle correctly.

```ini
[Service]
ExecStart=/path/to/eth-watchtower-supervisor -foreground -config /path/to/config.json
Restart=always
```

### Tips and appreciations

***ETH/ERC20:*** 0x9b4FfDADD87022C8B7266e28ad851496115ffB48

***SOL:*** 68L4XzSbRUaNE4UnxEd8DweSWEoiMQi6uygzERZLbXDw

***BTC:*** bc1qkmzc6d49fl0edyeynezwlrfqv486nmk6p5pmta

