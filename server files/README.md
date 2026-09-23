# Game Backend Server

A Go implementation of your mobile game backend API.

---

## What You Need

| Requirement | Details |
|---|---|
| **A VPS / Cloud Server** | Ubuntu 22.04 recommended. DigitalOcean, Hetzner, AWS EC2, Linode, Vultr all work. Min: 1 vCPU / 1 GB RAM |
| **Your domain** | You already have this ✓ |
| **Go 1.21+** | Install via `apt` or from golang.org |
| **GCC / build-essential** | Required to compile the SQLite driver (`go-sqlite3`) |
| **Nginx** | Reverse proxy + SSL termination |
| **Certbot** | Free SSL certificate (Let's Encrypt) |

---

## Endpoints Implemented

| Endpoint | Method | Description |
|---|---|---|
| `/api/v3/{userId}/{deviceUid}/user/login` | POST | Login / register user |
| `/api/v3/{userId}/{deviceUid}/packet` | POST | Execute batch commands+queries |
| `/health` | GET | Health check |

## 2.5.1 feature flags (v4 archive)

| Feature | Status |
|---------|--------|
| Daily login rewards | **On** — `user_data.daily_bonus` on login, `process_daily_reward` command |
| In-game leaderboards | **On** — `GET …/social_leaderboards` returns campaign + fast track boards |
| Multiplayer | **Off** — `fetch_flags.multiplayer_enabled: false`; race commands return `coming_soon` |

Flip `multiplayerEnabled()` in `leaderboards.go` when MP ships.

## Commands (inside `raw_packet`)
- `login` — Login, create user if new
- `process_daily_reward` — Claim daily login gem calendar
- `session_validate` — Check if session is valid
- `add_link` — Link identity provider account
- `confirmLink` — Confirm/cancel a link
- `analytics.heartbeat` — Heartbeat ping
- `analytics.finish` — Session length report
- `process_payment` — iTunes/Google Play payment
- `report_exception` — Client exception dump
- `report_crash` — Client crash dump
- `sync` — Store arbitrary key/value data
- `dl.command.sync` — Update user state
- `log_exception` — Log exception with severity
- `track` — Track analytics events
- `unsafe_track` — Track events without auth
- `copy_state` — Copy user state between accounts

## Queries (inside `raw_packet`)
- `login_response`, `force_upgrade`, `linked_accounts`, `link_mapping`
- `external_ids_map`, `user_basic_info`, `app_data`, `user`, `config`
- `last_tracked_events`, `last_tracked_users`

---

## Setup (Ubuntu 22.04)

### 1. Point your domain DNS
Add an **A record** pointing `yourdomain.com` (or `api.yourdomain.com`) to your server's IP address.

### 2. SSH into your server and install dependencies

```bash
sudo apt update && sudo apt upgrade -y
sudo apt install -y golang-go build-essential nginx certbot python3-certbot-nginx
```

> **Note:** `build-essential` (GCC) is required because the `go-sqlite3` driver uses CGo.

If you need Go 1.21+ and your distro ships an older version, install directly from golang.org:

```bash
wget https://go.dev/dl/go1.21.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

### 3. Upload and build the server

```bash
# Create app directory
sudo mkdir -p /opt/game_server
sudo chown $USER:$USER /opt/game_server

# Copy your files there
cp main.go database.go go.mod go.sum gameData.json /opt/game_server/

# Build the binary
cd /opt/game_server
go mod tidy
go build -o game_server .
```

### 4. Test it runs locally first

```bash
cd /opt/game_server
./game_server
# Press Ctrl+C when confirmed working
```

### 5. Set up Nginx

```bash
sudo tee /etc/nginx/sites-available/game_server > /dev/null <<'EOF'
server {
    listen 80;
    server_name yourdomain.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
EOF

# Edit: replace yourdomain.com with your real domain
sudo nano /etc/nginx/sites-available/game_server

# Enable it
sudo ln -s /etc/nginx/sites-available/game_server /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx
```

### 6. Get free SSL certificate

```bash
sudo certbot --nginx -d yourdomain.com
sudo systemctl reload nginx
```

### 7. Install as a system service (auto-start on reboot)

```bash
sudo tee /etc/systemd/system/game_server.service > /dev/null <<'EOF'
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
EOF

sudo systemctl daemon-reload
sudo systemctl enable game_server
sudo systemctl start game_server

# Check it's running
sudo systemctl status game_server
sudo journalctl -u game_server -f   # live logs
```

---

## Building Locally (Development)

```bash
make deps    # go mod tidy
make build   # compiles to ./game_server
make run     # build + run
```

---

## API Usage Examples

### Login
```bash
curl -X POST https://yourdomain.com/api/v3/1001/device-abc-123/user/login \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 1001,
    "platform": "ios",
    "security_token": "aaaabbbbccccddddeeeeffffgggghhhh",
    "client_version": 42,
    "client_bundle_version": "1.2.3",
    "client_ip": "1.2.3.4",
    "client_language": "en",
    "username": "PlayerOne",
    "device_os": "iOS 17.0",
    "device_model": "iPhone 15"
  }'
```

**Response:**
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

### Packet Execution (batch commands + queries)
```bash
curl -X POST https://yourdomain.com/api/v3/1001/device-abc-123/packet \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 1001,
    "session_id": 2847563918,
    "raw_packet": "{\"commands\":[{\"name\":\"sync\",\"args\":{\"user_id\":1001,\"key\":\"gold\",\"value\":500}}],\"queries\":[{\"name\":\"user\",\"filters\":{\"userId\":1001}}]}"
  }'
```

**Response:**
```json
{
  "commands": [
    {"name": "sync", "ok": true, "key": "gold"}
  ],
  "queries": [
    {"name": "user", "ok": true, "user": {...}, "state": {}}
  ]
}
```

---

## Database
The server uses **SQLite** (`game_server.db`) stored in `/opt/game_server/` via the `go-sqlite3` driver (CGo). For production at scale, you can swap `database.go` to use PostgreSQL with the `pgx` driver.

---

## Security Checklist
- [x] HTTPS via Let's Encrypt (auto-renews)
- [x] Session validation on all packet requests
- [x] Nginx rate limiting (add `limit_req_zone` to nginx.conf if needed)
- [ ] Add `SECRET_KEY` env var for token signing (optional enhancement)
- [ ] Add IP allowlisting for admin endpoints (optional)

---

## File Structure
```
/opt/game_server/
├── main.go            # HTTP server + all route/command/query handlers
├── database.go        # SQLite database layer
├── go.mod             # Go module definition
├── go.sum             # Dependency checksums
├── gameData.json      # Static game data
├── game_server        # Compiled binary (after build)
└── game_server.db     # SQLite database (auto-created on first run)
```
