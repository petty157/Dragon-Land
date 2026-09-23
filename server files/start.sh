#!/bin/bash
# ─────────────────────────────────────────────────────────────
#  Dragon Land Game Backend — Linux launch script
#  Usage: ./start.sh
#
#  Edit the variables below, then run:
#    chmod +x start.sh
#    ./start.sh
#
#  To run in the background (survives SSH disconnect):
#    tmux new -s gameserver
#    ./start.sh
#    # Detach: Ctrl+B then D
#    # Reattach: tmux attach -t gameserver
# ─────────────────────────────────────────────────────────────

# ── EDIT THIS ────────────────────────────────────────────────
export DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/1496448045095714886/2i0ldO3AUOAwadspJvDtNWTeCw3QWvu7RPSAd3_HA5A5J7U9xDenX3WjbS0pJF4GUgEi"

# Optional — change the default "secret" used to protect /bot/* endpoints
# export BOT_SECRET="your_secret_here"
# ─────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [ ! -f "./game_server" ]; then
    echo "game_server binary not found — building..."
    go build -o game_server .
fi

echo "Starting Dragon Land Game Backend..."
echo "Webhook: ${DISCORD_WEBHOOK_URL:0:50}..."
exec ./game_server
