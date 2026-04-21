# new-api-status

Lightweight, read-only status page for [new-api](https://github.com/QuantumNous/new-api). Polls the `logs` table every 30 seconds and renders per-group / per-channel / per-model availability as 60-minute bar charts.

**Zero writes to the database. Zero impact on new-api performance.**

## Features

- Per-group request availability (last 60 min)
- Per-channel status cards, click to expand per-model details
- Responsive 3-column grid layout
- Dark mode support
- Optional Cloudflare Turnstile verification
- Single binary with embedded UI (~15 MB)
- Supports PostgreSQL / MySQL / SQLite (same DB as new-api)

## Quick Start (Docker Compose)

Assumes new-api is already running with its own `docker-compose.yml` and a Docker network named `new-api_new-api-network`.

```yaml
services:
  new-api-status:
    build: .
    # or use: image: ghcr.io/uk0/new-api-status:latest
    container_name: new-api-status
    restart: unless-stopped
    ports:
      - "8787:8787"
    environment:
      - SQL_DSN=postgresql://root:123456@postgres:5432/new-api?sslmode=disable
      - DB_DRIVER=postgres
      - POLL_INTERVAL=30
      # - TURNSTILE_SITE_KEY=your-site-key
      # - TURNSTILE_SECRET_KEY=your-secret-key
    networks:
      - newapi-net

networks:
  newapi-net:
    external: true
    name: new-api_new-api-network  # must match new-api's network
```

```bash
docker compose up -d
# open http://localhost:8787
```

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `SQL_DSN` | Yes | - | Database connection string |
| `DB_DRIVER` | No | auto-detect | `postgres`, `mysql`, or `sqlite3` |
| `LISTEN` | No | `:8787` | HTTP listen address |
| `POLL_INTERVAL` | No | `30` | Database poll interval (seconds) |
| `TURNSTILE_SITE_KEY` | No | - | Cloudflare Turnstile site key |
| `TURNSTILE_SECRET_KEY` | No | - | Cloudflare Turnstile secret key |

## DSN Examples

```bash
# PostgreSQL
SQL_DSN=postgresql://user:pass@host:5432/new-api?sslmode=disable

# MySQL
SQL_DSN=user:pass@tcp(host:3306)/new-api

# SQLite
SQL_DSN=/path/to/new-api.db
```

## Build from Source

```bash
go build -o new-api-status .
./new-api-status -dsn "postgresql://..." 
```

## How It Works

1. Every `POLL_INTERVAL` seconds, runs a single `SELECT` on the `logs` table (last 60 min, type 2=success / type 5=error) with a `LEFT JOIN` on `channels` for names
2. Aggregates results in memory by group, channel name, and model name into per-minute buckets
3. Serves the pre-computed JSON via `GET /api/status`
4. Embedded single-page UI fetches and renders the data

The query uses existing indexes (`created_at`, `type`) and is bounded to 60 minutes of data, keeping it fast even on large tables.

## API

```
GET /api/status/config  → { "turnstileSiteKey": "..." }
GET /api/status         → { "groups": {...}, "channels": {...}, "updatedAt": 1234567890 }
```

## License

MIT
