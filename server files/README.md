# Game Backend Server

A Go-based backend server for a mobile game, implementing the API required for user authentication, state synchronization, analytics, rewards, leaderboards, and other game services.

## Features

* User login and registration
* Session validation
* Player state synchronization
* Daily login rewards
* In-game leaderboards
* Account linking
* Analytics and heartbeat tracking
* Crash and exception reporting
* Payment processing endpoints
* Batch command and query processing
* SQLite database storage
* Nginx reverse proxy support
* HTTPS with Let's Encrypt
* systemd service support

> Multiplayer functionality is currently disabled and can be enabled when the required backend implementation is ready.

---

## API

| Endpoint                                  | Method | Description                  |
| ----------------------------------------- | ------ | ---------------------------- |
| `/api/v3/{userId}/{deviceUid}/user/login` | `POST` | Login or register a user     |
| `/api/v3/{userId}/{deviceUid}/packet`     | `POST` | Execute commands and queries |
| `/health`                                 | `GET`  | Server health check          |

### Supported Commands

Commands are sent through the `raw_packet` field.

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

### Supported Queries

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

---

## 2.5.1 Feature Set

The current backend includes the following archived 2.5.1-era functionality:

| Feature              | Status   |
| -------------------- | -------- |
| Daily login rewards  | Enabled  |
| In-game leaderboards | Enabled  |
| Multiplayer          | Disabled |

Daily rewards are handled through `user_data.daily_bonus` and the `process_daily_reward` command.

Leaderboards are exposed through the `social_leaderboards` endpoint and currently provide campaign and fast-track boards.

Multiplayer-related commands currently return `coming_soon`.

To enable multiplayer once implemented, update `multiplayerEnabled()` in `leaderboards.go`.

---

## Requirements

| Requirement | Details                   |
| ----------- | ------------------------- |
| OS          | Ubuntu 22.04+ recommended |
| CPU         | 1 vCPU minimum            |
| RAM         | 1 GB minimum              |
| Go          | 1.21+                     |
| GCC         | Required by `go-sqlite3`  |
| Nginx       | Reverse proxy / HTTPS     |
| Certbot     | Let's Encrypt SSL         |

The server can be hosted on providers such as DigitalOcean, Hetzner, AWS EC2, Linode, or Vultr.

You will also need a domain pointing to your server.

---

# Installation

## 1. Clone the repository

```bash
git clone https://github.com/YOUR_USERNAME/YOUR_REPOSITORY.git
cd YOUR_REPOSITORY
```

## 2. Install dependencies

```bash
sudo apt update
sudo apt install -y golang-go build-essential nginx certbot python3-certbot-nginx
```

Check your Go version:

```bash
go version
```

Go 1.21 or newer is recommended.

If your distribution provides an older version, install Go directly from the official Go distribution.

---

## 3. Build the server

Install the Go dependencies:

```bash
go mod tidy
```

Build the server:

```bash
go build -o game_server .
```

---

## 4. Run locally

Start the server:

```bash
./game_server
```

The backend should start listening on its configured port.

You can then test the health endpoint:

```bash
curl http://127.0.0.1:8080/health
```

Press `Ctrl+C` to stop the server.

---

# Production Deployment

## 1. Create the application directory

```bash
sudo mkdir -p /opt/game_server
sudo chown $USER:$USER /opt/game_server
```

Copy or clone the repository into the directory:

```bash
cd /opt
git clone https://github.com/YOUR_USERNAME/YOUR_REPOSITORY.git game_server
cd game_server
```

Build it:

```bash
go mod tidy
go build -o game_server .
```

---

## 2. Configure DNS

Create an `A` record for your domain pointing to your server's public IP.

For example:

```text
api.example.com → YOUR_SERVER_IP
```

Make sure DNS has propagated before requesting the SSL certificate.

---

## 3. Configure Nginx

Create a site configuration:

```bash
sudo nano /etc/nginx/sites-available/game_server
```

Example:

```nginx
server {
    listen 80;
    server_name api.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;

        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Enable the configuration:

```bash
sudo ln -s /etc/nginx/sites-available/game_server /etc/nginx/sites-enabled/game_server
```

Test Nginx:

```bash
sudo nginx -t
```

Reload:

```bash
sudo systemctl reload nginx
```

---

## 4. Enable HTTPS

Run Certbot:

```bash
sudo certbot --nginx -d api.example.com
```

Certbot will configure the Let's Encrypt certificate and HTTPS automatically.

Test the deployment:

```bash
curl https://api.example.com/health
```

---

# systemd Service

To automatically start the backend after a reboot, create:

```bash
sudo nano /etc/systemd/system/game_server.service
```

Use:

```ini
[Unit]
Description=Game Backend Server
After=network.target

[Service]
Type=simple
User=www-data
WorkingDirectory=/opt/game_server
ExecStart=/opt/game_server/game_server
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Then enable and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable game_server
sudo systemctl start game_server
```

Check the service:

```bash
sudo systemctl status game_server
```

View live logs:

```bash
sudo journalctl -u game_server -f
```

---

# API Examples

## Login

```bash
curl -X POST https://api.example.com/api/v3/1001/device-abc-123/user/login \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 1001,
    "platform": "ios",
    "security_token": "aaaabbbbccccddddeeeeffffgggghhhh",
    "client_version": 42,
    "client_bundle_version": "1.2.3",
    "client_language": "en",
    "username": "PlayerOne",
    "device_os": "iOS 17.0",
    "device_model": "iPhone 15"
  }'
```

Example response:

```json
{
  "ok": true,
  "user_id": 1001,
  "session_id": 2847563918,
  "username": "PlayerOne",
  "platform": "ios",
  "created_at": 1713312000
}
```

## Packet Execution

Commands and queries can be sent together through `raw_packet`:

```bash
curl -X POST https://api.example.com/api/v3/1001/device-abc-123/packet \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 1001,
    "session_id": 2847563918,
    "raw_packet": "{\"commands\":[{\"name\":\"sync\",\"args\":{\"user_id\":1001,\"key\":\"gold\",\"value\":500}}],\"queries\":[{\"name\":\"user\",\"filters\":{\"userId\":1001}}]}"
  }'
```

Example response:

```json
{
  "commands": [
    {
      "name": "sync",
      "ok": true,
      "key": "gold"
    }
  ],
  "queries": [
    {
      "name": "user",
      "ok": true,
      "user": {},
      "state": {}
    }
  ]
}
```

---

# Database

The backend currently uses SQLite through the `go-sqlite3` driver.

The database is automatically created in the application directory:

```text
game_server.db
```

For larger deployments, the database layer can be replaced with PostgreSQL using the `pgx` driver.

---

# Development

If the repository includes the provided Makefile:

```bash
make deps
make build
make run
```

Equivalent Go commands:

```bash
go mod tidy
go build -o game_server .
./game_server
```

---

# Security

Current security features:

* HTTPS support through Let's Encrypt
* Session validation on packet requests
* Nginx reverse proxy
* Optional Nginx rate limiting

Recommended future improvements:

* [ ] Add a `SECRET_KEY` environment variable for token signing
* [ ] Add authentication for administrative endpoints
* [ ] Add IP allowlisting for admin functionality
* [ ] Add stricter request validation
* [ ] Add production database backups
* [ ] Add monitoring and alerting

---

# Project Structure

```text
game_server/
├── main.go             # HTTP server and API handlers
├── database.go         # SQLite database layer
├── go.mod              # Go module definition
├── go.sum              # Dependency checksums
├── gameData.json       # Static game data
├── game_server         # Compiled binary
└── game_server.db      # SQLite database (created automatically)
```

---

## License

MIT
