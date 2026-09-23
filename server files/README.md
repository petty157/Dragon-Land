# Game Backend Server

Go implementation of the mobile game backend API.

## Requirements

* Go 1.21+
* GCC / `build-essential` (required by `go-sqlite3`)
* SQLite

## Setup

Clone the repository:

```bash
git clone https://github.com/YOUR_USERNAME/YOUR_REPO.git
cd YOUR_REPO
```

Install dependencies:

```bash
go mod tidy
```

Build:

```bash
go build -o game_server .
```

Run:

```bash
./game_server
```

The server will start on its configured port.

### Development

You can also use:

```bash
make deps
make build
make run
```

## API

### Login

```text
POST /api/v3/{userId}/{deviceUid}/user/login
```

### Packet

```text
POST /api/v3/{userId}/{deviceUid}/packet
```

Packets contain commands and queries inside the `raw_packet` field.

### Health Check

```text
GET /health
```

Example:

```bash
curl http://localhost:8080/health
```

## Supported Commands

* `login`
* `process_daily_reward`
* `session_validate`
* `add_link`
* `confirmLink`
* `analytics.heartbeat`
* `analytics.finish`
* `process_payment`
* `report_exception`
* `report_crash`
* `sync`
* `dl.command.sync`
* `log_exception`
* `track`
* `unsafe_track`
* `copy_state`

## Supported Queries

* `login_response`
* `force_upgrade`
* `linked_accounts`
* `link_mapping`
* `external_ids_map`
* `user_basic_info`
* `app_data`
* `user`
* `config`
* `last_tracked_events`
* `last_tracked_users`

## Database

The server uses SQLite.

The database is automatically created as:

```text
game_server.db
```

## Project Structure

```text
├── main.go
├── database.go
├── go.mod
├── go.sum
├── gameData.json
└── README.md
```

## Notes

This project is primarily intended for game preservation, testing, and backend research.
