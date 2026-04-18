# qbittorrent-matrix

A Matrix bot that monitors a qBittorrent instance and posts notifications to a room when torrents are added, complete, start/stop seeding, or encounter errors. It also responds to commands for on-demand status queries.

## Commands

| Command | Description |
|---|---|
| `!list` | All torrents with status, progress, size, speeds, ratio, and activity |
| `!download` / `!downloading` | Torrents actively downloading, with progress, speed, and ETA |
| `!uploading` | Torrents actively uploading (speed > 0), with upload speed and ratio |

## Automatic notifications

The bot posts to the room when:

- A new torrent is added
- A download completes
- A torrent starts or stops seeding
- A torrent enters an error state
- A torrent is removed

## Configuration

Copy the sample config and fill in your values:

```bash
cp sample.config.yaml config.yaml
```

Then edit `config.yaml`:

```yaml
matrix:
  homeserver: "https://matrix.example.com"
  user_id: "@yourbot:matrix.example.com"
  # Provide either access_token OR username+password.
  # If access_token is set, username/password are ignored.
  access_token: ""
  username: "yourbot"
  password: "changeme"
  room_id: "!roomid:matrix.example.com"

qbittorrent:
  url: "http://localhost:8080"
  username: "admin"
  password: "adminadmin"
  # How often to poll qBittorrent for changes, in seconds.
  poll_interval: 30
  # HTTP Basic Auth credentials for a reverse proxy in front of qBittorrent
  # (e.g. nginx/openresty). Leave empty if accessing qBittorrent directly.
  http_username: ""
  http_password: ""
```

**Matrix credentials** — you need a Matrix account for the bot. Either:
- Set `access_token` (recommended): obtain one from your Matrix client under Settings → Help & About → Access Token.
- Or set `username` + `password` and leave `access_token` empty.

**qBittorrent** — the bot uses the qBittorrent WebUI API. Make sure the WebUI is enabled in qBittorrent preferences. If qBittorrent sits behind a reverse proxy with HTTP Basic Auth, fill in `http_username` and `http_password`.

## Running with Docker (recommended)

With `config.yaml` ready, pull and start the container:

```bash
docker compose up -d
```

To pick up a new release:

```bash
docker compose pull && docker compose up -d
```

## Running from source

Requirements: Go 1.25+

```bash
go build -o qbittorrent-matrix .
./qbittorrent-matrix          # uses config.yaml in the current directory
./qbittorrent-matrix /path/to/config.yaml
```
