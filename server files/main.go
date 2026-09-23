package main

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var db *Database

// gameConfig holds raw gamedata.json — never re-parsed so key order is preserved.
var gameConfig json.RawMessage

// suspiciousAlertSent tracks the last time a suspicious-activity alert was fired
// for each user, so we don't spam Discord more than once per hour per player.
var (
	suspiciousAlertMu   sync.Mutex
	suspiciousAlertSent = make(map[int64]time.Time)
)

// pendingVerification holds a 6-digit code for a Discord user trying to link
// their account to a game user_id. Expires after 5 minutes.
// originalLives is the player's real _lives value before we overwrote it with
// the code so we can restore it on verify or expiry.
type pendingVerif struct {
	code          string
	gameUID       int64
	expiresAt     time.Time
	originalLives int64
	attempts      int // wrong attempts so far; locked out after 3
}

var (
	pendingVerifMu    sync.Mutex
	pendingVerifCodes = make(map[string]pendingVerif) // key = discord_id
)

// registerRateLimit tracks how many /register attempts a Discord user has made
// in the past hour so we can block abuse.
var (
	registerRateMu    sync.Mutex
	registerAttempts  = make(map[string][]time.Time) // key = discord_id
)

// ── Discord webhook ────────────────────────────────────────────────────────
// Set DISCORD_WEBHOOK_URL as an env var before launching the server:
//   export DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/..."
//   ./game_server
// Or use the provided start.sh which does this automatically.
// Leave it unset to disable all Discord notifications.
//
// discordWebhookURL is read at runtime (not package init) so it works
// correctly whether you export the var in your shell, use start.sh,
// or set it via systemd Environment=.
var discordWebhookURL string

// discordEmbed sends a rich embed to the configured webhook (non-blocking).
func discordEmbed(title, description string, color int, fields []map[string]interface{}) {
	if discordWebhookURL == "" {
		return
	}
	embed := map[string]interface{}{
		"title":       title,
		"description": description,
		"color":       color,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	body, _ := json.Marshal(map[string]interface{}{"embeds": []interface{}{embed}})
	go func() {
		resp, err := http.Post(discordWebhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("Discord webhook error: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

// ── Helpers ────────────────────────────────────────────────────────────────

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func makeSessionID() int64 {
	var b [4]byte
	rand.Read(b[:])
	return int64(binary.BigEndian.Uint32(b[:]))
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func jsonWrite(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// mustParseInt64 converts a numeric string to int64; returns 0 on error.
func mustParseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// displayName returns the player's username, or "User#<id>" when no name is set yet.
func displayName(userID int64, name string) string {
	if strings.TrimSpace(name) == "" {
		return fmt.Sprintf("User#%d", userID)
	}
	return name
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg})
}

// readBody decompresses gzip if Content-Encoding says so.
func readBody(r *http.Request) ([]byte, error) {
	reader := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(reader)
}

// extractPathParams parses /{prefix}/api/v3/{userID}/{deviceUID}/suffix
func extractPathParams(path string) (userID int64, deviceUID string, suffix string) {
	if idx := strings.Index(path, "/api/v3/"); idx >= 0 {
		path = path[idx:]
	}
	trimmed := strings.TrimPrefix(path, "/api/v3/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 3 {
		return 0, "", ""
	}
	uid, _ := strconv.ParseInt(parts[0], 10, 64)
	return uid, parts[1], parts[2]
}

// parseArgsFromBody tries JSON first, then falls back to form-encoded.
// The game's /user/login uses application/x-www-form-urlencoded.
func parseArgsFromBody(r *http.Request, body []byte) map[string]interface{} {
	args := make(map[string]interface{})
	ct := r.Header.Get("Content-Type")

	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		vals, err := url.ParseQuery(string(body))
		if err == nil {
			for k, v := range vals {
				if len(v) == 1 {
					args[k] = v[0]
				} else {
					args[k] = v
				}
			}
		}
		return args
	}

	// Default: try JSON — UseNumber so int64 IDs don't lose precision via float64
	if len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		dec.Decode(&args)
	}
	return args
}

// ── Packet types ────────────────────────────────────────────────────────────

type RealCommand struct {
	CID  string                 `json:"cid"`
	Cmd  string                 `json:"cmd"`
	TS   int64                  `json:"ts"`
	Args map[string]interface{} `json:"args"`
}

type RealQuery struct {
	QID     string                 `json:"qid"`
	Name    string                 `json:"name"`
	Filters map[string]interface{} `json:"filters"`
}

type RealPacket struct {
	PID      int64         `json:"pid"`
	TS       int64         `json:"ts"`
	Commands []RealCommand `json:"commands"`
	Queries  []RealQuery   `json:"queries"`
}

type RealPacketResponse struct {
	PID      int64                    `json:"pid"`
	TS       int64                    `json:"ts"`
	Commands []map[string]interface{} `json:"commands"`
	Queries  []map[string]interface{} `json:"queries"`
}

// ── Command handlers ───────────────────────────────────────────────────────

func cmdLoginInner(args map[string]interface{}, pathUserID int64, pathDeviceUID string) (int64, int64, bool, error) {
	uid := pathUserID
	if v := getInt64(args, "user_id"); v != 0 {
		uid = v
	}
	platform := getStr(args, "platform")
	securityToken := getStr(args, "security_token")

	isNew, err := db.EnsureUser(uid)
	if err != nil {
		return 0, 0, false, err
	}
	db.TouchLogin(uid)
	log.Printf("Login: user=%d platform=%s isNew=%v", uid, platform, isNew)

	// Notify Discord when a brand-new player registers.
	if isNew {
		total, _ := db.GetTotalUsers()
		discordEmbed(
			"🎮 New Player Registered!",
			fmt.Sprintf("Player `%d` just joined Dragon Land for the first time.", uid),
			0x2ecc71, // green
			[]map[string]interface{}{
				{"name": "Platform", "value": platform, "inline": true},
				{"name": "Total Players", "value": fmt.Sprintf("%d", total), "inline": true},
			},
		)
		// Send in-game inbox message telling the new player to link their Discord.
		db.SendInboxMessage(uid, 0, "InboxMessageSystemNews",
			map[string]interface{}{
				"sender":  "Dragon Land",
				"subject": "⚠️ Protect Your Account",
				"body":    "Link your Discord account so you never lose your progress! Complete your first level, then go to our Discord server and type /register " + fmt.Sprintf("%d", uid) + " — a 6-digit code will appear here in your inbox to confirm the link. If you ever reinstall, use /login on Discord to recover everything.",
				"rewards": []interface{}{},
			},
		)
	}

	sessionID := makeSessionID()
	db.CreateSession(uid, sessionID, securityToken, platform, time.Now().Unix())
	db.UpsertDevice(uid, pathDeviceUID,
		getStr(args, "device_os"),
		getStr(args, "device_model"),
		platform,
	)
	return uid, sessionID, isNew, nil
}

func cmdLogin(args map[string]interface{}, pathUserID int64, pathDeviceUID string) map[string]interface{} {
	uid, sessionID, _, err := cmdLoginInner(args, pathUserID, pathDeviceUID)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	return map[string]interface{}{"ok": true, "user_id": uid, "session_id": sessionID}
}

func cmdSessionValidate(args map[string]interface{}, userID, sessionID int64) map[string]interface{} {
	valid := db.ValidateSession(userID, sessionID)
	return map[string]interface{}{"ok": valid, "valid": valid}
}

// cmdSync — game sends key="data", value={full player state} on every packet.
func cmdSync(args map[string]interface{}, userID int64) map[string]interface{} {
	uid := userID
	if v := getInt64(args, "user_id"); v != 0 {
		uid = v
	}
	// Drop writes for wiped accounts — prevents stale client from overwriting migrated state.
	if _, exists, _ := db.GetPlayerByID(uid); !exists {
		log.Printf("SYNC DROPPED: user=%d no longer exists (wiped/migrated)", uid)
		return map[string]interface{}{"ok": true}
	}
	key := getStr(args, "key")
	if key == "data" {
		// ── Verif-code guard ──────────────────────────────────────────────────
		// If this player has an active verification code, their _lives is
		// currently set to the 6-digit code. The game will try to sync that
		// value back as if it were real lives. We intercept the state blob and
		// clamp _lives to originalLives so the code value can never be saved as
		// the player's actual lives count, preventing any exploit.
		pendingVerifMu.Lock()
		var activePV *pendingVerif
		for _, pv := range pendingVerifCodes {
			if pv.gameUID == uid {
				copy := pv
				activePV = &copy
				break
			}
		}
		pendingVerifMu.Unlock()

		if activePV != nil {
			if stateMap, ok := args["value"].(map[string]interface{}); ok {
				incomingLives := getInt64(stateMap, "_lives")
				codeAsInt := mustParseInt64(activePV.code)
				// Only clamp if the game is trying to write the code value itself
				// as lives. If the player spent/gained lives normally (different
				// value), let that through — but still cap at originalLives so
				// they can't inflate lives during the verif window either.
				if incomingLives == codeAsInt || incomingLives > activePV.originalLives {
					stateMap["_lives"] = activePV.originalLives
					args["value"] = stateMap
					log.Printf("SYNC user=%d: clamped _lives %d → %d (verif code active)", uid, incomingLives, activePV.originalLives)
				}
			}
		}

		raw := jsonMarshal(args["value"])
		log.Printf("SYNC user=%d key=%s stateLen=%d", uid, key, len(raw))
		if err := db.SaveUserState(uid, raw); err != nil {
			log.Printf("SaveUserState error user=%d: %v", uid, err)
		}
		// Auto-update campaign leaderboard from the synced state blob.
		if stateMap, ok := args["value"].(map[string]interface{}); ok {
			syncLeaderboardFromState(uid, stateMap)
		}
	} else {
		log.Printf("SYNC user=%d key=%s (skipped — not 'data')", uid, key)
	}
	return map[string]interface{}{"ok": true, "key": key}
}

// syncLeaderboardFromState extracts _scoreMP and _maxLevelCompleted from the
// player state blob and upserts them into the campaign leaderboard so that the
// leaderboard stays current even when the game doesn't send an explicit
// update_campaign_leaderboard command.
func syncLeaderboardFromState(userID int64, state map[string]interface{}) {
	// _scoreMP is the primary campaign score the leaderboard displays.
	if score := getInt64(state, "_scoreMP"); score > 0 {
		if _, err := db.UpsertCampaignScore("CAMPAIGN", userID, score); err != nil {
			log.Printf("syncLeaderboardFromState CAMPAIGN error user=%d: %v", userID, err)
		}
	}
	// MicroserieMaxCoins drives the Fast Track leaderboard — coins collected, not campaign level.
	// Do NOT fall back to _maxLevelReached — that is campaign node number, not coins.
	ftCoins := getInt64(state, "MicroserieMaxCoins")
	if ftCoins > 0 {
		if _, err := db.UpsertCampaignScore("FAST_TRACK", userID, ftCoins); err != nil {
			log.Printf("syncLeaderboardFromState FAST_TRACK error user=%d: %v", userID, err)
		}
	}

	// ── Suspicious activity detection ──────────────────────────────────────
	checkSuspiciousActivity(userID, state)
}

// checkSuspiciousActivity inspects a player's synced state for impossible or
// absurd values and fires a Discord alert if anything looks cheated.
// Alerts fire at most once per player per hour to avoid webhook spam.
// Whitelisted players are silently skipped.
func checkSuspiciousActivity(userID int64, state map[string]interface{}) {
	// Whitelisted players are allowed to have any values — no alerts.
	if db.IsWhitelisted(userID) {
		return
	}

	// If this user has a pending verification, their _lives is temporarily set
	// to the 6-digit code — skip the lives check to avoid a false positive.
	pendingVerifMu.Lock()
	hasPendingVerif := false
	for _, pv := range pendingVerifCodes {
		if pv.gameUID == userID {
			hasPendingVerif = true
			break
		}
	}
	pendingVerifMu.Unlock()

	type flag struct {
		field     string
		value     int64
		threshold int64
		label     string
	}

	checks := []flag{
		{"_coins",          getInt64(state, "_coins"),          5_000_000,  "Coins"},
		{"_gems",           getInt64(state, "_gems"),           100_000,    "Gems"},
		{"_scoreMP",        getInt64(state, "_scoreMP"),        50_000_000, "Campaign Score"},
		{"_maxLevelReached",getInt64(state, "_maxLevelReached"),10_000,     "Max Level"},
		{"_keys",           getInt64(state, "_keys"),           10_000,     "Keys"},
	}
	// Only flag lives if there's no active verification code set on this player.
	if !hasPendingVerif {
		checks = append(checks, flag{"_lives", getInt64(state, "_lives"), 9_999, "Lives"})
	}

	var violations []map[string]interface{}
	for _, c := range checks {
		if c.value > c.threshold {
			violations = append(violations, map[string]interface{}{
				"name":   c.label,
				"value":  fmt.Sprintf("%d (threshold: %d)", c.value, c.threshold),
				"inline": true,
			})
		}
	}

	if len(violations) == 0 {
		suspiciousAlertMu.Lock()
		delete(suspiciousAlertSent, userID) // clear flag when player is clean
		suspiciousAlertMu.Unlock()
		return
	}

	// Rate-limit: only alert once per hour per player
	suspiciousAlertMu.Lock()
	lastAlert, alreadyFlagged := suspiciousAlertSent[userID]
	if alreadyFlagged && time.Since(lastAlert) < time.Hour {
		suspiciousAlertMu.Unlock()
		return
	}
	suspiciousAlertSent[userID] = time.Now()
	suspiciousAlertMu.Unlock()

	name, _ := db.GetUserName(userID)
	dn := name
	if dn == "" {
		dn = fmt.Sprintf("%d", userID)
	}

	log.Printf("SUSPICIOUS user=%d violations=%d", userID, len(violations))

	fields := append([]map[string]interface{}{
		{"name": "Player", "value": fmt.Sprintf("`%d`", userID), "inline": true},
		{"name": "Username", "value": dn, "inline": true},
	}, violations...)

	discordEmbed(
		"⚠️ Suspicious Activity Detected",
		"A player has values exceeding normal gameplay thresholds. Possible cheating or save manipulation.",
		0xe74c3c,
		fields,
	)
}

// cmdDLCommandSync — dl.command.sync updates user state fields directly.
func cmdDLCommandSync(args map[string]interface{}, userID int64) map[string]interface{} {
	uid := userID
	if v := getInt64(args, "user_id"); v != 0 {
		uid = v
	}
	// Drop writes for wiped accounts — same guard as cmdSync.
	if _, exists, _ := db.GetPlayerByID(uid); !exists {
		log.Printf("DLCommandSync DROPPED: user=%d no longer exists (wiped/migrated)", uid)
		return map[string]interface{}{"ok": true}
	}
	key := getStr(args, "key")
	if key == "" {
		key = "data"
	}
	raw := jsonMarshal(args["value"])
	if raw == "" || raw == "null" {
		return map[string]interface{}{"ok": true, "key": key}
	}
	log.Printf("DLCommandSync user=%d key=%s len=%d", uid, key, len(raw))
	if err := db.SaveUserState(uid, raw); err != nil {
		log.Printf("DLCommandSync SaveUserState error user=%d: %v", uid, err)
	}
	return map[string]interface{}{"ok": true, "key": key}
}

func cmdEvent(args map[string]interface{}, userID, sessionID int64) map[string]interface{} {
	eventType := getStr(args, "type")
	if eventType == "" {
		eventType = "unknown"
	}
	log.Printf("Event (not stored): user=%d type=%s", userID, eventType)
	return map[string]interface{}{"ok": true}
}

func cmdTrack(args map[string]interface{}, userID, sessionID int64) map[string]interface{} {
	events, _ := args["events"].([]interface{})
	log.Printf("Track (not stored): user=%d count=%d", userID, len(events))
	return map[string]interface{}{"ok": true, "tracked": len(events)}
}

func cmdCopyState(args map[string]interface{}, userID, sessionID int64) map[string]interface{} {
	fromUID := getInt64(args, "from")
	toUID := getInt64(args, "to")
	if toUID == 0 {
		toUID = userID
	}
	if !db.ValidateSession(toUID, sessionID) {
		return map[string]interface{}{"ok": false, "error": "invalid_session"}
	}
	db.CopyUserState(fromUID, toUID)
	return map[string]interface{}{"ok": true, "from": fromUID, "to": toUID}
}

func cmdPushEnabled(args map[string]interface{}, userID int64) map[string]interface{} {
	token := getStr(args, "token")
	if token == "" {
		return map[string]interface{}{"ok": false, "error": "missing token"}
	}
	if err := db.UpsertPushToken(userID, token); err != nil {
		log.Printf("UpsertPushToken error user=%d: %v", userID, err)
	}
	return map[string]interface{}{"ok": true}
}

func cmdFetchFlags(args map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"ok": true,
		"flags": map[string]interface{}{
			"tournament_enabled":  true,
			"multiplayer_enabled": false,
			"gacha_enabled":       true,
			"offer_wall_enabled":  true,
			"video_ads_enabled":   false,
			"cross_promo_enabled": false,
			"leaderboard_enabled": true,
			"daily_bonus_enabled": true,
		},
	}
}

func cmdSendLife(args map[string]interface{}) map[string]interface{} {
	friendID := getInt64(args, "friend_id")
	senderID := getInt64(args, "user_id")
	if friendID == 0 {
		return map[string]interface{}{"ok": false, "error": "missing friend_id"}
	}
	if _, exists, _ := db.GetPlayerByID(friendID); !exists {
		return map[string]interface{}{"ok": false, "error": "user_not_found"}
	}
	db.SendInboxMessage(friendID, senderID, "InboxMessageLifeReceived", map[string]interface{}{
		"senderId": fmt.Sprintf("%d", senderID),
		"sender":   fmt.Sprintf("%d", senderID),
		"rewards":  []interface{}{},
	})
	log.Printf("SendLife: user=%d -> friend=%d", senderID, friendID)
	return map[string]interface{}{"ok": true}
}

func cmdLogException(args map[string]interface{}) map[string]interface{} {
	log.Printf("log_exception: channel=%s severity=%s", getStr(args, "channel"), getStr(args, "severity"))
	return map[string]interface{}{"ok": true}
}

// cmdSetUserName — sets the display name for a user.
// DLL: ChangeUsernameCommandId / SendSetUserNameCommand
// Args: user_id, name (new username)
func cmdSetUserName(args map[string]interface{}, userID int64) map[string]interface{} {
	uid := userID
	if v := getInt64(args, "user_id"); v != 0 {
		uid = v
	}
	name := getStr(args, "name")
	if name == "" {
		name = getStr(args, "username")
	}
	if name == "" {
		return map[string]interface{}{"ok": true} // no name provided, nothing to do
	}
	// Skip DB write if name hasn't changed — prevents relaunch from resetting username
	if existing, _ := db.GetUserName(uid); existing == name {
		return map[string]interface{}{"ok": true, "user_id": uid, "name": name}
	}
	if err := db.SetUserName(uid, name); err != nil {
		log.Printf("SetUserName error user=%d: %v", uid, err)
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}

	// Also update _userName in the state blob so the game reflects the new name
	// immediately without needing a relaunch (game reads _userName from state).
	if stateRaw, err := db.GetUserState(uid); err == nil && stateRaw != "" {
		stateMap := map[string]interface{}{}
		dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
		dec.UseNumber()
		if dec.Decode(&stateMap) == nil {
			stateMap["_userName"] = name
			_ = db.SaveUserState(uid, jsonMarshal(stateMap))
		}
	}

	log.Printf("SetUserName: user=%d name=%s", uid, name)
	discordEmbed(
		"✏️ Player Name Set",
		fmt.Sprintf("Player `%d` set their username to **%s**.", uid, name),
		0x3498db,
		nil,
	)
	return map[string]interface{}{"ok": true, "user_id": uid, "name": name}
}

// cmdUpdateCampaignLeaderboard — game sends this after completing levels.
// HAR shows: args = {user_id, node, score}
// leaderboard_id for campaign nodes = "CAMPAIGN"
func cmdUpdateCampaignLeaderboard(args map[string]interface{}, userID int64) map[string]interface{} {
	uid := userID
	if v := getInt64(args, "user_id"); v != 0 {
		uid = v
	}

	// The game uses leaderboard_id OR a fixed "CAMPAIGN" board indexed by node
	lbID := getStr(args, "leaderboard_id")
	if lbID == "" {
		lbID = "CAMPAIGN"
	}
	score := getInt64(args, "score")
	node := getInt64(args, "node")

	rank, err := db.UpsertCampaignScore(lbID, uid, score)
	if err != nil {
		log.Printf("CampaignLeaderboard upsert error: %v", err)
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	log.Printf("CampaignLeaderboard: user=%d lb=%s node=%d score=%d rank=%d", uid, lbID, node, score, rank)
	return map[string]interface{}{
		"ok":    true,
		"rank":  rank,
		"score": score,
	}
}

// cmdUpdateFastTrackLeaderboard — same shape as campaign but for Fast Track mode.
// DLL: CommandUpdateFastTrackLBId/Score/UserId
func cmdUpdateFastTrackLeaderboard(args map[string]interface{}, userID int64) map[string]interface{} {
	uid := userID
	if v := getInt64(args, "user_id"); v != 0 {
		uid = v
	}
	lbID := getStr(args, "leaderboard_id")
	if lbID == "" {
		lbID = "FAST_TRACK"
	}
	score := getInt64(args, "score")

	rank, err := db.UpsertCampaignScore(lbID, uid, score)
	if err != nil {
		log.Printf("FastTrackLeaderboard upsert error: %v", err)
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	log.Printf("FastTrackLeaderboard: user=%d lb=%s score=%d rank=%d", uid, lbID, score, rank)
	return map[string]interface{}{
		"ok":    true,
		"rank":  rank,
		"score": score,
	}
}

// cmdSyncTournament — upserts tournament score and returns leaderboard.
func cmdSyncTournament(args map[string]interface{}, userID int64) map[string]interface{} {
	category := getStr(args, "category")
	if category == "" {
		category = "default"
	}
	score := getInt64(args, "score")
	goals, _ := args["goals"].(map[string]interface{})
	if goals == nil {
		goals = map[string]interface{}{}
	}

	t, err := db.EnsureTournament(category)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	db.UpsertTournamentScore(t.ID, userID, score, goals)

	entries, _ := db.GetTournamentLeaderboard(t.ID, 100)
	ranking := make([]map[string]interface{}, 0, len(entries))
	userRank := int64(len(entries))
	for _, e := range entries {
		ranking = append(ranking, map[string]interface{}{
			"user_id": strconv.FormatInt(e.UserID, 10),
			"score":   e.Score,
			"rank":    e.Rank,
		})
		if e.UserID == userID {
			userRank = e.Rank
		}
	}
	return map[string]interface{}{
		"ok":            true,
		"tournament_id": t.ID,
		"category":      category,
		"rank":          userRank,
		"ranking":       ranking,
		"ends_at":       t.EndsAt,
	}
}

// cmdChangeTournamentCategory — player picks a tournament tier (bronze/silver/gold).
// DLL: ChangeCategoryCommandId / SentChangeTournamentCategory
func cmdChangeTournamentCategory(args map[string]interface{}, userID int64) map[string]interface{} {
	category := getStr(args, "category")
	if category == "" {
		category = "default"
	}
	t, err := db.EnsureTournament(category)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	log.Printf("ChangeTournamentCategory: user=%d category=%s tournamentID=%d", userID, category, t.ID)
	return map[string]interface{}{
		"ok":            true,
		"tournament_id": t.ID,
		"category":      category,
		"starts_at":     t.StartsAt,
		"ends_at":       t.EndsAt,
	}
}

// cmdStartRace — sent when a multiplayer race begins.
// DLL: StartRaceCommandId, args: userId, dragon, skin, level
// We record the race start so we can validate finish_race later.
func cmdStartRace(args map[string]interface{}, userID int64) map[string]interface{} {
	if !multiplayerEnabled() {
		return multiplayerDisabledResponse()
	}
	level := getStr(args, "level")
	dragon := getStr(args, "dragon")
	skin := getStr(args, "skin")
	log.Printf("StartRace: user=%d level=%s dragon=%s skin=%s", userID, level, dragon, skin)
	// Persist a lightweight "race in progress" marker in user state.
	stateRaw, _ := db.GetUserState(userID)
	state := map[string]interface{}{}
	if stateRaw != "" {
		func() {
		dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
		dec.UseNumber()
		dec.Decode(&state)
	}()
	}
	state["_raceInProgress"] = map[string]interface{}{
		"level":   level,
		"dragon":  dragon,
		"skin":    skin,
		"started": nowTs(),
	}
	db.SaveUserState(userID, jsonMarshal(state))
	return map[string]interface{}{"ok": true}
}

// cmdFinishRace — sent when a multiplayer race ends.
// DLL: EndRaceCommandId / finish_race
// args (from DLL symbols):
//
//	EndRaceCommandAttrUserIdKey  = userId
//	EndRaceCommandAttrRankingKey = ranking  (1-based finish position)
//	EndRaceCommandAttrDragonKey  = dragon
//	EndRaceCommandAttrLevelKey   = level
//	EndRaceCommandAttrSkinKey    = skin
//
// Points awarded by position (matches original game balance):
//
//	1st = 10, 2nd = 7, 3rd = 5, 4th = 3
func cmdFinishRace(args map[string]interface{}, userID int64) map[string]interface{} {
	if !multiplayerEnabled() {
		return multiplayerDisabledResponse()
	}
	ranking := int(getInt64(args, "ranking"))
	level := getStr(args, "level")
	dragon := getStr(args, "dragon")
	log.Printf("FinishRace: user=%d ranking=%d level=%s dragon=%s", userID, ranking, level, dragon)

	// Points per finishing position.
	pointsTable := map[int]int64{1: 10, 2: 7, 3: 5, 4: 3}
	points, ok := pointsTable[ranking]
	if !ok {
		points = 3 // fallback for unexpected position
	}

	// --- Update user state: winsInARow, totalMPWins ---
	stateRaw, _ := db.GetUserState(userID)
	state := map[string]interface{}{}
	if stateRaw != "" {
		func() {
		dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
		dec.UseNumber()
		dec.Decode(&state)
	}()
	}
	// Clear in-progress marker.
	delete(state, "_raceInProgress")

	// Track win streak (DLL: _multiplayerWinsInARow).
	winsInARow := getInt64(state, "_multiplayerWinsInARow")
	if ranking == 1 {
		winsInARow++
	} else {
		winsInARow = 0
	}
	state["_multiplayerWinsInARow"] = winsInARow

	// Total MP wins (DLL: WIN_MULTIPLAYER).
	totalWins := getInt64(state, "_multiplayerWins")
	if ranking == 1 {
		totalWins++
	}
	state["_multiplayerWins"] = totalWins
	db.SaveUserState(userID, jsonMarshal(state))

	// --- Update MP leaderboard (campaign score for MP_GLOBAL / MP_FRIENDS). ---
	// The game uses cumulative points, so we add to the current score.
	currentScore, _ := db.GetCampaignScore("MP_GLOBAL", userID)
	newScore := currentScore + points
	rank, _ := db.UpsertCampaignScore("MP_GLOBAL", userID, newScore)

	// --- Update active tournament score if user is in one. ---
	t, err := db.EnsureTournament("default")
	if err == nil {
		currentTournamentScore, _ := db.GetTournamentScore(t.ID, userID)
		db.UpsertTournamentScore(t.ID, userID, currentTournamentScore+points, nil)
	}

	log.Printf("FinishRace: user=%d position=%d points=%d newMPScore=%d rank=%d winsInARow=%d",
		userID, ranking, points, newScore, rank, winsInARow)

	return map[string]interface{}{
		"ok":             true,
		"points_earned":  points,
		"total_mp_score": newScore,
		"rank":           rank,
		"wins_in_a_row":  winsInARow,
	}
}

// cmdChangeRacerCategory — player changed their tournament tier in the MP lobby.
// DLL: ChangeCategoryCommandId, args: category, dragon, level, skin
func cmdChangeRacerCategory(args map[string]interface{}, userID int64) map[string]interface{} {
	if !multiplayerEnabled() {
		return multiplayerDisabledResponse()
	}
	category := getStr(args, "category")
	dragon := getStr(args, "dragon")
	level := getStr(args, "level")
	log.Printf("ChangeRacerCategory: user=%d category=%s dragon=%s level=%s", userID, category, dragon, level)
	t, err := db.EnsureTournament(category)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	return map[string]interface{}{
		"ok":            true,
		"tournament_id": t.ID,
		"category":      category,
	}
}

// cmdNewInstall — sent once when the game is freshly installed.
// DLL: NewInstallCommandId
func cmdNewInstall(args map[string]interface{}, userID int64) map[string]interface{} {
	log.Printf("NewInstall: user=%d platform=%s", userID, getStr(args, "platform"))
	return map[string]interface{}{"ok": true}
}

// cmdRefreshFriends — asks server to push any pending friend-related commands.
// DLL: RefreshFriendsCommandId
func cmdRefreshFriends(args map[string]interface{}, userID int64) map[string]interface{} {
	ids, _ := db.GetFriends(userID)
	friends := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		friends = append(friends, map[string]interface{}{"user_id": strconv.FormatInt(id, 10)})
	}
	return map[string]interface{}{"ok": true, "friends": friends}
}

// cmdSocial handles friend add/remove/request operations.
func cmdSocial(args map[string]interface{}, userID int64) map[string]interface{} {
	action := getStr(args, "action")
	targetID := getInt64(args, "friend_id")
	if targetID == 0 {
		targetID = getInt64(args, "target_id")
	}

	switch action {
	case "add_friend", "accept_friend":
		if targetID == 0 {
			return map[string]interface{}{"ok": false, "error": "missing friend_id"}
		}
		db.AddFriend(userID, targetID, "sp")
		db.DeleteInboxMessage(getInt64(args, "request_id"), userID)
		log.Printf("Social: user=%d added friend=%d", userID, targetID)
		return map[string]interface{}{"ok": true, "action": action, "friend_id": targetID}

	case "remove_friend":
		if targetID == 0 {
			return map[string]interface{}{"ok": false, "error": "missing friend_id"}
		}
		db.RemoveFriend(userID, targetID)
		return map[string]interface{}{"ok": true, "action": "remove_friend"}

	case "send_friend_request":
		if targetID == 0 {
			return map[string]interface{}{"ok": false, "error": "missing friend_id"}
		}
		if _, exists, _ := db.GetPlayerByID(targetID); !exists {
			return map[string]interface{}{"ok": false, "error": "user_not_found"}
		}
		db.SendInboxMessage(targetID, userID, "InboxMessageOnlineFriendRequest", map[string]interface{}{
			"senderId": fmt.Sprintf("%d", userID),
			"sender":   fmt.Sprintf("%d", userID),
			"rewards":  []interface{}{},
		})
		return map[string]interface{}{"ok": true, "action": "send_friend_request"}

	default:
		return map[string]interface{}{"ok": true}
	}
}

func cmdSendFriendRequest(args map[string]interface{}, userID int64) map[string]interface{} {
	return cmdSocial(map[string]interface{}{
		"action":    "send_friend_request",
		"friend_id": args["friend_id"],
	}, userID)
}

func cmdRemoveFriend(args map[string]interface{}, userID int64) map[string]interface{} {
	return cmdSocial(map[string]interface{}{
		"action":    "remove_friend",
		"friend_id": args["friend_id"],
	}, userID)
}

func cmdAcceptFriendRequest(args map[string]interface{}, userID int64) map[string]interface{} {
	return cmdSocial(map[string]interface{}{
		"action":     "accept_friend",
		"friend_id":  args["from_user_id"],
		"request_id": args["request_id"],
	}, userID)
}

// cmdAskForLife — asks a friend to send a life. DLL: AskForLifeCommand.
func cmdAskForLife(args map[string]interface{}, userID int64) map[string]interface{} {
	friendID := getInt64(args, "friend_id")
	if friendID == 0 {
		return map[string]interface{}{"ok": false, "error": "missing friend_id"}
	}
	if _, exists, _ := db.GetPlayerByID(friendID); !exists {
		return map[string]interface{}{"ok": false, "error": "user_not_found"}
	}
	db.SendInboxMessage(friendID, userID, "InboxMessageLifeHelpRequest", map[string]interface{}{
		"senderId": fmt.Sprintf("%d", userID),
		"sender":   fmt.Sprintf("%d", userID),
		"rewards":  []interface{}{},
	})
	log.Printf("AskForLife: user=%d -> friend=%d", userID, friendID)
	return map[string]interface{}{"ok": true}
}

func cmdProcessPayment(args map[string]interface{}) map[string]interface{} {
	log.Printf("Payment: user=%d gateway=%s", getInt64(args, "user_id"), getStr(args, "gateway"))
	return map[string]interface{}{"ok": true, "order_id": randomHex(8), "gateway": getStr(args, "gateway")}
}

// Stubs — ack without persisting.
func cmdAddLink(args map[string]interface{}) map[string]interface{}            { return map[string]interface{}{"ok": true} }
func cmdConfirmLink(args map[string]interface{}) map[string]interface{}        { return map[string]interface{}{"ok": true} }
func cmdAnalyticsHeartbeat(args map[string]interface{}) map[string]interface{} { return map[string]interface{}{"ok": true} }
func cmdAnalyticsFinish(args map[string]interface{}) map[string]interface{}    { return map[string]interface{}{"ok": true} }
func cmdReportException(args map[string]interface{}) map[string]interface{}    { return map[string]interface{}{"ok": true} }
func cmdReportCrash(args map[string]interface{}) map[string]interface{}        { return map[string]interface{}{"ok": true} }

// buildInboxItem serialises an InboxMsg into the exact wire shape the client
// parser (InboxMessageData.parseElementData) expects. Field names come from
// the client DLL string heap — do not rename them.
//
//   type      — InboxMessageType enum string (required, must not be empty)
//   sender    — display name of the sender shown in the inbox UI
//   senderId  — numeric user_id of the sender as a string
//   subject   — bold title line shown in the message list
//   body      — message body text
//   rewards   — array of reward objects (empty array, never null/missing)
//
// IMPORTANT: "subject_ext" and "body_ext" must NEVER be sent as empty strings.
// The DLL's parseElementData iterates every key in the AttrDic and calls
// Enum.Parse on extension values, which throws ArgumentException on "".
// Omit these keys entirely when they have no value — the real SP server does
// not send them for plain messages.
func buildInboxItem(m InboxMsg) map[string]interface{} {
	senderStr := fmt.Sprintf("%d", m.FromUserID)
	if m.FromUserID == 0 {
		senderStr = "0"
	}

	// Start with the required fields the parser always reads.
	// Do NOT include subject_ext or body_ext here — they are added below
	// only when non-empty.
	item := map[string]interface{}{
		"id":         m.ID,
		"type":       m.Type,
		"sender":     senderStr,
		"senderId":   senderStr,
		"rewards":    []interface{}{},
		"created_at": m.CreatedAt,
	}
	// subject/body are required by ParseListValues for all message types.
	item["subject"] = ""
	item["body"] = ""

	// Overlay stored data — prefer the wire-correct field names written by new
	// code, but also accept the legacy "title"/"message" keys written by old code.
	// Skip "type" here: it comes from the validated DB column only, never from
	// the data blob. Old rows may have a stale/wrong "type" key in their blob
	// that would otherwise overwrite the correctly-migrated column value and
	// cause Enum.Parse to crash on the client.
	// Also skip subject_ext/body_ext from the blob — we re-add them below only
	// when non-empty, to avoid the Enum.Parse crash on empty strings.
	for k, v := range m.Data {
		if k == "type" || k == "subject_ext" || k == "body_ext" {
			continue
		}
		item[k] = v
	}

	// Always re-assert type from the validated DB column — this is the safety
	// net that guarantees the client never sees an empty or invalid type string.
	item["type"] = m.Type

	// All types (including "news") use "subject" and "body" as the wire JSON
	// keys. The DLL's ParseListValues reads those field names for every
	// InboxMessageType. "newsSubject" / "newsBody" are the C# *property*
	// names on InboxMessageData, not the JSON keys — the parser maps
	// subject→newsSubject and body→newsBody internally.
	//
	// Promote stored newsSubject/newsBody (written by old code) into the
	// correct wire keys, then remove the legacy camelCase keys.
	if sv, _ := item["subject"].(string); sv == "" {
		if t, ok := m.Data["newsSubject"].(string); ok && t != "" {
			item["subject"] = t
		} else if t, ok := m.Data["title"].(string); ok && t != "" {
			item["subject"] = t
		}
	}
	if bv, _ := item["body"].(string); bv == "" {
		if b, ok := m.Data["newsBody"].(string); ok && b != "" {
			item["body"] = b
		} else if b, ok := m.Data["message"].(string); ok && b != "" {
			item["body"] = b
		}
	}
	// Remove legacy camelCase keys — they are unknown to the DLL parser and
	// can trigger an Enum.Parse crash when the parser iterates all keys.
	delete(item, "newsSubject")
	delete(item, "newsBody")

	// sender display name: prefer explicit "sender" key, fall back to senderId.
	if sv, ok := item["sender"].(string); !ok || sv == "" || sv == "0" {
		item["sender"] = senderStr
	}

	// rewards must never be null — Enum.Parse-adjacent crash if missing.
	if item["rewards"] == nil {
		item["rewards"] = []interface{}{}
	}

	// Re-add subject_ext / body_ext ONLY when non-empty.
	// Sending them as "" causes Enum.Parse to throw ArgumentException in the DLL.
	if ext, ok := m.Data["subject_ext"].(string); ok && ext != "" {
		item["subject_ext"] = ext
	}
	if ext, ok := m.Data["body_ext"].(string); ok && ext != "" {
		item["body_ext"] = ext
	}

	// Final safety net: delete these keys if they ended up empty for ANY reason.
	// This guards against old DB rows or future code paths that might sneak in "".
	// An empty subject_ext / body_ext causes Enum.Parse to crash in the DLL.
	if v, ok := item["subject_ext"].(string); ok && v == "" {
		delete(item, "subject_ext")
	}
	if v, ok := item["body_ext"].(string); ok && v == "" {
		delete(item, "body_ext")
	}

	return item
}

// ── Query handlers ─────────────────────────────────────────────────────────

func qryLoginResponse(filters map[string]interface{}) map[string]interface{} {
	sid := getInt64(filters, "session_id")
	session, err := db.GetSessionByID(sid)
	if err != nil || session == nil {
		return map[string]interface{}{"ok": false, "error": "session_not_found"}
	}
	return map[string]interface{}{"ok": true, "session": session}
}

func qryForceUpgrade(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "force_upgrade": false}
}

func qryLinkedAccounts(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "linked_accounts": []interface{}{}}
}

func qryLinkMapping(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "mapping": map[string]interface{}{}}
}

func qryExternalIDsMap(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "external_ids_map": map[string]interface{}{}}
}

func qryUserBasicInfo(filters map[string]interface{}) map[string]interface{} {
	uid := getInt64(filters, "user_id")
	if uid == 0 {
		uid = getInt64(filters, "userId")
	}
	name, exists, _ := db.GetPlayerByID(uid)
	if !exists {
		return map[string]interface{}{"ok": false, "error": "user_not_found"}
	}
	return map[string]interface{}{"ok": true, "user": map[string]interface{}{"id": uid, "name": name}}
}

func qryAppData(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "app_data": nil}
}

func qryUser(filters map[string]interface{}) map[string]interface{} {
	uid := getInt64(filters, "userId")
	if _, exists, _ := db.GetPlayerByID(uid); !exists {
		return map[string]interface{}{"ok": false, "error": "user_not_found"}
	}
	userData, err := db.BuildUserData(uid)
	if err != nil || userData == nil {
		return map[string]interface{}{"ok": false, "error": "user_not_found"}
	}
	return map[string]interface{}{"ok": true, "user_data": userData}
}

func qryConfig(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "config": gameConfig}
}

func qryLastTrackedUsers(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"ok": true, "users": []interface{}{}}
}

// qrySocialLeaderboard — packet query + legacy alias; full UI uses GET social_leaderboards.
func qrySocialLeaderboard(filters map[string]interface{}) map[string]interface{} {
	userID := getInt64(filters, "user_id")
	if userID == 0 {
		return map[string]interface{}{"ok": false, "error": "missing user_id"}
	}
	out := buildSocialLeaderboardsResponse(userID, filters)
	out["ok"] = true
	return out
}

func qryGetRequests(filters map[string]interface{}) map[string]interface{} {
	userID := getInt64(filters, "receiverId")
	if userID == 0 {
		userID = getInt64(filters, "user_id")
	}
	msgs, err := db.GetInboxMessages(userID)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	result := make([]interface{}, 0, len(msgs))
	for _, m := range msgs {
		result = append(result, buildInboxItem(m))
	}
	return map[string]interface{}{"ok": true, "inbox": result, "total": len(result)}
}

func qryFriends(filters map[string]interface{}) map[string]interface{} {
	userID := getInt64(filters, "user_id")
	ids, _ := db.GetFriends(userID)
	friends := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		name, exists, _ := db.GetPlayerByID(id)
		if !exists {
			continue // skip deleted/banned users
		}
		friends = append(friends, map[string]interface{}{
			"user_id": strconv.FormatInt(id, 10),
			"name":    name,
		})
	}
	return map[string]interface{}{"ok": true, "friends": friends}
}

func qryTournamentInfo(filters map[string]interface{}) map[string]interface{} {
	category := getStr(filters, "category")
	if category == "" {
		category = "bronze"
	}
	t, err := db.EnsureTournament(category)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	remaining := t.EndsAt - time.Now().Unix()
	if remaining < 0 {
		remaining = 0
	}
	return map[string]interface{}{
		"current_id":        strconv.FormatInt(t.ID, 10),
		"remaining_seconds": remaining,
		"next_id":           strconv.FormatInt(t.ID, 10),
		"previous_id":       strconv.FormatInt(t.ID, 10),
		"category":          t.Category,
		"previous_category": t.Category,
		"win_streak":        0,
		"goals":             []interface{}{},
	}
}

// buildTournamentBlock builds a single leaderboard block in the exact shape
// the game expects inside tournament_leaderboard — matching api.php's $block.
func buildTournamentBlock(cat string, callerUID int64) map[string]interface{} {
	t, _ := db.EnsureTournament(cat)
	entries, _ := db.GetTournamentLeaderboard(t.ID, 100)
	now := time.Now().Unix()
	ranking := make([]map[string]interface{}, 0, len(entries))
	var me map[string]interface{}
	for _, e := range entries {
		name, _ := db.GetUserName(e.UserID)
		entry := map[string]interface{}{
			"id": e.UserID, "name": displayName(e.UserID, name),
			"score": e.Score, "position": e.Rank, "level": 0,
			"dragon": 0, "skin": 0, "category": cat,
			"promotion": 0, "node": 0,
			"last_high_score_at": now,
			"external_provider": "", "external_id": 0, "fake": false,
		}
		ranking = append(ranking, entry)
		if e.UserID == callerUID {
			me = entry
		}
	}
	if me == nil {
		name, _ := db.GetUserName(callerUID)
		me = map[string]interface{}{
			"id": callerUID, "name": displayName(callerUID, name),
			"score": 0, "position": int64(len(ranking) + 1), "level": 0,
			"dragon": 0, "skin": 0, "category": cat,
			"promotion": 0, "node": 0,
			"last_high_score_at": now,
			"external_provider": "", "external_id": 0, "fake": false,
		}
	}
	return map[string]interface{}{"ranking": ranking, "user": me}
}

func qryAllTournaments(filters map[string]interface{}) map[string]interface{} {
	uid := getInt64(filters, "user_id")
	// 7 blocks: GLOBAL, BRONZE, SILVER, GOLD, PREV_BRONZE, PREV_SILVER, PREV_GOLD
	cats := []string{"default", "bronze", "silver", "gold", "bronze", "silver", "gold"}
	blocks := make([]map[string]interface{}, 0, 7)
	for _, cat := range cats {
		blocks = append(blocks, buildTournamentBlock(cat, uid))
	}
	return map[string]interface{}{"tournament_leaderboard": blocks}
}

// qryCurrentTournamentInfo — exact translation of api.php current_tournament_info block
func qryCurrentTournamentInfo(filters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"current_id":        "t1",
		"remaining_seconds": 86400 * 3,
		"next_id":           "t1",
		"previous_id":       "t1",
		"category":          "bronze",
		"previous_category": "bronze",
		"win_streak":        0,
		"goals":             []interface{}{},
	}
}

// qryAllTournamentInfo — exact translation of api.php all_tournament_info block.
// Uses real user data from DB instead of the hardcoded DragonLandPlayer placeholder.
func qryAllTournamentInfo(filters map[string]interface{}) map[string]interface{} {
	uid := getInt64(filters, "user_id")
	name, _ := db.GetUserName(uid)
	score, _ := db.GetCampaignScore("CAMPAIGN", uid)
	now := time.Now().Unix()

	me := map[string]interface{}{
		"id":                 uid,
		"name":               displayName(uid, name),
		"score":              score,
		"position":           1,
		"level":              10,
		"dragon":             0,
		"skin":               0,
		"category":           "bronze",
		"promotion":          0,
		"node":               0,
		"last_high_score_at": now,
		"external_provider":  "",
		"external_id":        0,
		"fake":               false,
	}

	block := map[string]interface{}{
		"ranking": []interface{}{me},
		"user":    me,
	}

	// 7 blocks: GLOBAL, BRONZE, SILVER, GOLD, PREV_BRONZE, PREV_SILVER, PREV_GOLD
	blocks := make([]interface{}, 7)
	for i := range blocks {
		blocks[i] = block
	}

	return map[string]interface{}{
		"tournament_leaderboard": blocks,
	}
}

// ── Packet executor ────────────────────────────────────────────────────────

var queryHandlers = map[string]func(map[string]interface{}) map[string]interface{}{
	"login_response":      qryLoginResponse,
	"force_upgrade":       qryForceUpgrade,
	"linked_accounts":     qryLinkedAccounts,
	"link_mapping":        qryLinkMapping,
	"external_ids_map":    qryExternalIDsMap,
	"user_basic_info":     qryUserBasicInfo,
	"app_data":            qryAppData,
	"user":                qryUser,
	"config":              qryConfig,
	"last_tracked_users":  qryLastTrackedUsers,
	"social_leaderboard":  qrySocialLeaderboard, // singular — matches what the game sends
	"social_leaderboards": qrySocialLeaderboard, // plural alias kept for safety
	"get_requests":        qryGetRequests,
	"friends":             qryFriends,
	"tournament_info":           qryTournamentInfo,
	"all_tournaments":           qryAllTournaments,
	"current_tournament_info":   qryCurrentTournamentInfo,
	"all_tournament_info":       qryAllTournamentInfo,
}

func executeRealPacket(pkt RealPacket, userID, sessionID int64, deviceUID string) RealPacketResponse {
	// Update last_login_at on every packet so online-player detection stays accurate.
	// The game sends packets constantly (heartbeat, sync, etc.) so this is a good proxy.
	db.TouchLogin(userID)

	resp := RealPacketResponse{
		PID:      pkt.PID,
		TS:       time.Now().Unix(),
		Commands: []map[string]interface{}{},
		Queries:  []map[string]interface{}{},
	}

	for _, cmd := range pkt.Commands {
		entry := map[string]interface{}{
			"cid": cmd.CID,
			"cmd": cmd.Cmd,
		}

		var res map[string]interface{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					res = map[string]interface{}{"ok": false, "error": fmt.Sprintf("%v", r)}
				}
			}()
			switch cmd.Cmd {
			case "login":
				res = cmdLogin(cmd.Args, userID, deviceUID)
			case "session_validate":
				res = cmdSessionValidate(cmd.Args, userID, sessionID)
			case "sync":
				res = cmdSync(cmd.Args, userID)
			case "dl.command.sync", "dl.sync":
				res = cmdDLCommandSync(cmd.Args, userID)
			case "event":
				res = cmdEvent(cmd.Args, userID, sessionID)
			case "track":
				res = cmdTrack(cmd.Args, userID, sessionID)
			case "copy_state":
				res = cmdCopyState(cmd.Args, userID, sessionID)
			case "push_enabled":
				res = cmdPushEnabled(cmd.Args, userID)
			case "fetch_flags":
				res = cmdFetchFlags(cmd.Args)
			case "send_life":
				res = cmdSendLife(cmd.Args)
			case "log_exception":
				res = cmdLogException(cmd.Args)
			case "add_link":
				res = cmdAddLink(cmd.Args)
			case "confirmLink", "confirm_link":
				res = cmdConfirmLink(cmd.Args)
			case "analytics.heartbeat":
				res = cmdAnalyticsHeartbeat(cmd.Args)
			case "analytics.finish":
				res = cmdAnalyticsFinish(cmd.Args)
			case "process_payment":
				res = cmdProcessPayment(cmd.Args)
			case "report_exception":
				res = cmdReportException(cmd.Args)
			case "report_crash":
				res = cmdReportCrash(cmd.Args)
			case "process_daily_reward":
				res = cmdProcessDailyReward(cmd.Args, userID)
			// ── Leaderboards ──────────────────────────────────────────────
			case "update_campaign_leaderboard":
				res = cmdUpdateCampaignLeaderboard(cmd.Args, userID)
			case "update_fast_track_leaderboard":
				res = cmdUpdateFastTrackLeaderboard(cmd.Args, userID)
			// ── Tournament ────────────────────────────────────────────────
			case "sync_tournament":
				res = cmdSyncTournament(cmd.Args, userID)
			case "change_tournament_category", "change_category":
				res = cmdChangeTournamentCategory(cmd.Args, userID)
			// ── User ──────────────────────────────────────────────────────
			case "set_user_name", "change_username", "rename_if_exists":
				res = cmdSetUserName(cmd.Args, userID)
			// ── Social ────────────────────────────────────────────────────
			case "social", "social_command":
				res = cmdSocial(cmd.Args, userID)
			case "send_friend_request":
				res = cmdSendFriendRequest(cmd.Args, userID)
			case "remove_friend", "remove_friendship":
				res = cmdRemoveFriend(cmd.Args, userID)
			case "accept_friend_request", "client_add_request":
				res = cmdAcceptFriendRequest(cmd.Args, userID)
			case "ask_for_life":
				res = cmdAskForLife(cmd.Args, userID)
			// ── Misc ──────────────────────────────────────────────────────
			case "new_install":
				res = cmdNewInstall(cmd.Args, userID)
			case "refresh_friends":
				res = cmdRefreshFriends(cmd.Args, userID)
			// ── Stubs for commands DLL sends but server doesn't need to fully process ──
			case "update_goal", "finish_goal":
				// Tournament goal progress — ack without persisting (no goal DB yet)
				log.Printf("%s: user=%d goalId=%s", cmd.Cmd, userID, getStr(cmd.Args, "goal_id"))
				res = map[string]interface{}{"ok": true}
			case "start_race":
				res = cmdStartRace(cmd.Args, userID)
			case "finish_race", "end_race":
				res = cmdFinishRace(cmd.Args, userID)
			case "change_racer_category":
				res = cmdChangeRacerCategory(cmd.Args, userID)
			case "client_process_request":
				// Process inbox message (accept/decline friend requests sent via inbox)
				requestID := getInt64(cmd.Args, "request_id")
				isAccepted, _ := cmd.Args["is_accepted"].(bool)
				if isAccepted {
					fromUID := getInt64(cmd.Args, "sender_id")
					if fromUID == 0 {
						fromUID = getInt64(cmd.Args, "from_user_id")
					}
					if fromUID != 0 {
						db.AddFriend(userID, fromUID, "sp")
					}
				}
				db.DeleteInboxMessage(requestID, userID)
				res = map[string]interface{}{"ok": true}
			case "xpromo", "trigger_custom_response", "multiplayer_room_closed":
				res = map[string]interface{}{"ok": true}
			default:
				log.Printf("Unknown command: %q user=%d", cmd.Cmd, userID)
				res = map[string]interface{}{"ok": true}
			}
		}()

		for k, v := range res {
			entry[k] = v
		}
		resp.Commands = append(resp.Commands, entry)
	}

	// Execute queries embedded in the packet (the game sends these alongside commands).
	for _, qry := range pkt.Queries {
		entry := map[string]interface{}{
			"qid":  qry.QID,
			"name": qry.Name,
		}
		filters := qry.Filters
		if filters == nil {
			filters = map[string]interface{}{}
		}
		// Inject the authenticated user_id so query handlers can use it.
		if _, ok := filters["user_id"]; !ok {
			filters["user_id"] = userID
		}
		var res map[string]interface{}
		if handler, ok := queryHandlers[qry.Name]; ok {
			func() {
				defer func() {
					if r := recover(); r != nil {
						res = map[string]interface{}{"ok": false, "error": fmt.Sprintf("%v", r)}
					}
				}()
				res = handler(filters)
			}()
		} else {
			log.Printf("Unknown query: %q user=%d", qry.Name, userID)
			res = map[string]interface{}{"ok": true}
		}
		for k, v := range res {
			entry[k] = v
		}
		resp.Queries = append(resp.Queries, entry)
	}

	return resp
}

// ── Login response ─────────────────────────────────────────────────────────

func buildLoginResponse(uid, sessionID int64, isNew bool) (map[string]interface{}, error) {
	now := time.Now().Unix()

	userData, err := db.BuildUserData(uid)
	if err != nil {
		return nil, fmt.Errorf("BuildUserData: %w", err)
	}

	if isNew {
		userData["_firstSession"] = true
	}
	attachLoginUserMeta(userData, uid)

	// game_data.user is userData without the "id" field.
	gameUser := make(map[string]interface{}, len(userData))
	for k, v := range userData {
		if k != "id" {
			gameUser[k] = v
		}
	}

	// Load pending inbox messages and inject them into game_data.inbox.
	inboxMsgs, _ := db.GetInboxMessages(uid)
	inboxItems := make([]interface{}, 0, len(inboxMsgs))
	for _, m := range inboxMsgs {
		inboxItems = append(inboxItems, buildInboxItem(m))
	}

	return map[string]interface{}{
		"generic_data": map[string]interface{}{
			"ts": strconv.FormatInt(now, 10),
		},
		"login_data": map[string]interface{}{
			"user_id":           strconv.FormatInt(uid, 10),
			"session_id":        strconv.FormatInt(sessionID, 10),
			"ts":                strconv.FormatInt(now, 10),
			"expire":            86400,
			"forced_upgrade":    false,
			"suggested_upgrade": false,
			"user_importance":   "normal",
		},
		"user_data": userData,
		"game_data": map[string]interface{}{
			"config": gameConfig,
			"inbox":  inboxItems,
			"user":   gameUser,
		},
	}, nil
}

// ── HTTP handlers ──────────────────────────────────────────────────────────

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	userID, deviceUID, _ := extractPathParams(r.URL.Path)

	body, _ := readBody(r)
	args := parseArgsFromBody(r, body)

	log.Printf("Login: userID=%d deviceUID=%s", userID, deviceUID)

	// Blacklist check — block banned devices before any DB writes.
	if db.IsBlacklisted(userID, deviceUID) {
		log.Printf("Login BLOCKED (device blacklisted): user=%d device=%s", userID, deviceUID)
		jsonError(w, http.StatusForbidden, "account_banned")
		return
	}

	// Soft-ban check — block banned accounts (data preserved, login refused).
	if db.IsBanned(userID) {
		_, banUntilTs, _ := db.GetBanInfo(userID)
		log.Printf("Login BLOCKED (soft-banned): user=%d ban_until=%d", userID, banUntilTs)
		jsonWrite(w, map[string]interface{}{
			"ok":        false,
			"error":     "account_banned",
			"ban_until": banUntilTs,
		})
		return
	}

	uid, sessionID, isNew, err := cmdLoginInner(args, userID, deviceUID)
	if err != nil {
		log.Printf("Login error: %v", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp, err := buildLoginResponse(uid, sessionID, isNew)
	if err != nil {
		log.Printf("buildLoginResponse error: %v", err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, resp)
}

func handlePacket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	userID, deviceUID, _ := extractPathParams(r.URL.Path)

	sessionID, _ := strconv.ParseInt(r.URL.Query().Get("session_id"), 10, 64)
	if sessionID == 0 || !db.ValidateSession(userID, sessionID) {
		log.Printf("Packet rejected: user=%d session=%d", userID, sessionID)
		jsonError(w, http.StatusUnauthorized, "invalid_session")
		return
	}

	body, err := readBody(r)
	if err != nil {
		log.Printf("Packet readBody error: %v", err)
		jsonError(w, http.StatusBadRequest, "bad_request")
		return
	}
	log.Printf("Packet: user=%d session=%d bodyLen=%d", userID, sessionID, len(body))

	// Handle empty body — game sometimes sends a bare heartbeat packet
	if len(body) == 0 || string(body) == "{}" {
		jsonWrite(w, RealPacketResponse{TS: time.Now().Unix(), Commands: []map[string]interface{}{}})
		return
	}

	var pkt RealPacket
	decPkt := json.NewDecoder(bytes.NewReader(body))
	decPkt.UseNumber()
	if err := decPkt.Decode(&pkt); err != nil {
		log.Printf("Packet JSON error: %v — body: %s", err, string(body[:min(200, len(body))]))
		jsonError(w, http.StatusBadRequest, "invalid_json")
		return
	}

	jsonWrite(w, executeRealPacket(pkt, userID, sessionID, deviceUID))
}

func handleTrack(w http.ResponseWriter, r *http.Request) {
	userID, _, _ := extractPathParams(r.URL.Path)
	body, _ := readBody(r)
	var data map[string]interface{}
	if json.Unmarshal(body, &data) == nil {
		events, _ := data["events"].([]interface{})
		log.Printf("Track (not stored): user=%d count=%d", userID, len(events))
	}
	jsonWrite(w, map[string]interface{}{"ok": true})
}

func handleUnauthorizedTrack(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var data map[string]interface{}
	if json.Unmarshal(body, &data) == nil {
		events, _ := data["events"].([]interface{})
		log.Printf("UnauthorizedTrack (not stored): count=%d", len(events))
	}
	jsonWrite(w, map[string]interface{}{"ok": true})
}

// handleQuery handles GET /query/* endpoints.
func handleQuery(w http.ResponseWriter, r *http.Request) {
	userID, _, _ := extractPathParams(r.URL.Path)
	q := r.URL.Query()

	switch {
	// ── Social leaderboards (in-game ranking UI) ─────────────────────────
	case strings.Contains(r.URL.Path, "social_leaderboards"):
		filters := map[string]interface{}{"user_id": userID}
		for k, v := range q {
			filters[k] = v
		}
		jsonWrite(w, buildSocialLeaderboardsResponse(userID, filters))

	// ── Campaign leaderboard (direct GET) ────────────────────────────────
	case strings.Contains(r.URL.Path, "leaderboard"):
		lbID := q.Get("leaderboard_id")
		if lbID == "" {
			lbID = q.Get("id")
		}
		if lbID == "" {
			// Detect CAMPAIGN vs FAST_TRACK from path
			if strings.Contains(r.URL.Path, "fast_track") || strings.Contains(r.URL.Path, "FAST_TRACK") {
				lbID = "FAST_TRACK"
			} else {
				lbID = "CAMPAIGN"
			}
		}
		entries, total, err := db.GetCampaignLeaderboard(lbID, userID, 100)
		if err != nil {
			jsonWrite(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		ranking := make([]map[string]interface{}, 0, len(entries))
		for _, e := range entries {
			name, _ := db.GetUserName(e.UserID)
			ranking = append(ranking, map[string]interface{}{
				"user_id": e.UserID,
				"name":    displayName(e.UserID, name),
				"score":   e.Score,
				"rank":    e.Rank,
			})
		}
		jsonWrite(w, map[string]interface{}{
			"ok":      true,
			"ranking": ranking,
			"total":   total,
		})

	// ── Inbox / requests ─────────────────────────────────────────────────
	// DLL calls: GET /get_requests?receiverId={receiverId}&session_id={sessionId}
	// Response key the game reads: "inbox" (same structure as login game_data.inbox)
	case strings.Contains(r.URL.Path, "get_requests"):
		receiverID, _ := strconv.ParseInt(q.Get("receiverId"), 10, 64)
		if receiverID == 0 {
			receiverID = userID
		}
		msgs, _ := db.GetInboxMessages(receiverID)
		inboxItems := make([]interface{}, 0, len(msgs))
		for _, m := range msgs {
			inboxItems = append(inboxItems, buildInboxItem(m))
		}
		jsonWrite(w, map[string]interface{}{
			"ok":    true,
			"inbox": inboxItems,
			"total": len(inboxItems),
		})

	// ── Tournament info (direct GET, not via /query/) ─────────────────
	// DLL calls: GET /current_tournament_info?user_id={userId}
	//            GET /all_tournament_info?user_id={userId}
	case strings.Contains(r.URL.Path, "current_tournament_info"):
		category := q.Get("category")
		if category == "" {
			category = "bronze"
		}
		jsonWrite(w, qryCurrentTournamentInfo(map[string]interface{}{"category": category}))

	case strings.Contains(r.URL.Path, "all_tournament_info"):
		jsonWrite(w, qryAllTournamentInfo(map[string]interface{}{"user_id": userID}))

	// ── Tournament queries (via /query/) ────────────────────────────────
	case strings.Contains(r.URL.Path, "tournament"):
		category := q.Get("category")
		if strings.Contains(r.URL.Path, "all") {
			jsonWrite(w, qryAllTournamentInfo(map[string]interface{}{"user_id": userID}))
		} else {
			jsonWrite(w, qryCurrentTournamentInfo(map[string]interface{}{"category": category}))
		}

	// ── Friends ──────────────────────────────────────────────────────────
	case strings.Contains(r.URL.Path, "friends"):
		jsonWrite(w, qryFriends(map[string]interface{}{"user_id": userID}))

	// ── Username availability ─────────────────────────────────────────
	case strings.Contains(r.URL.Path, "is_username_available"):
		username := q.Get("username")
		// We don't enforce uniqueness — just confirm it's available so the game proceeds
		jsonWrite(w, map[string]interface{}{"ok": true, "available": true, "username": username})

	default:
		log.Printf("Query: user=%d path=%s (unhandled)", userID, r.URL.Path)
		jsonWrite(w, map[string]interface{}{
			"ok":       true,
			"requests": []interface{}{},
			"data":     map[string]interface{}{},
		})
	}
}

// handleBotSearchPlayer — GET …/bot/search_player?q=username OR ?user_id=123
// Used by the Discord bot /player command to search by name or ID.
func handleBotSearchPlayer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Search by user ID
	if uidStr := q.Get("user_id"); uidStr != "" {
		uid, err := strconv.ParseInt(uidStr, 10, 64)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid_user_id")
			return
		}
		name, exists, err := db.GetPlayerByID(uid)
		if err != nil {
			log.Printf("GetPlayerByID error user=%d: %v", uid, err)
		}
		if !exists {
			jsonWrite(w, map[string]interface{}{"ok": true, "results": []interface{}{}})
			return
		}
		jsonWrite(w, map[string]interface{}{
			"ok": true,
			"results": []map[string]interface{}{
				{"user_id": strconv.FormatInt(uid, 10), "username": displayName(uid, name)},
			},
		})
		return

	}

	// Search by username (partial match)
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		jsonError(w, http.StatusBadRequest, "missing_query")
		return
	}
	matches, err := db.SearchPlayerByUsername(query)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	results := make([]map[string]interface{}, 0, len(matches))
	for _, m := range matches {
		results = append(results, map[string]interface{}{
			"user_id":  strconv.FormatInt(m.UserID, 10),
			"username": m.Username,
		})
	}
	jsonWrite(w, map[string]interface{}{"ok": true, "results": results})
}

// handleBotPlayerStats — GET …/bot/player_stats?user_id=123
// Returns a player's in-game stats from their state blob for the Discord bot.
func handleBotPlayerStats(w http.ResponseWriter, r *http.Request) {
	uidStr := r.URL.Query().Get("user_id")
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	// Verify the user actually exists — banned/deleted accounts must return not found.
	if _, exists, _ := db.GetPlayerByID(uid); !exists {
		jsonWrite(w, map[string]interface{}{"ok": true, "found": false})
		return
	}
	stateRaw, err := db.GetUserState(uid)
	if err != nil || stateRaw == "" {
		jsonWrite(w, map[string]interface{}{"ok": true, "found": false})
		return
	}
	state := map[string]interface{}{}
	if err := func() error {
		dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
		dec.UseNumber()
		return dec.Decode(&state)
	}(); err != nil {
		jsonWrite(w, map[string]interface{}{"ok": true, "found": false})
		return
	}
	_, banUntil, isBanned := db.GetBanInfo(uid)
	jsonWrite(w, map[string]interface{}{
		"ok":                true,
		"found":             true,
		"coins":             getInt64(state, "_coins"),
		"gems":              getInt64(state, "_gems"),
		"score_mp":          getInt64(state, "_scoreMP"),
		"max_level_reached": getInt64(state, "_maxLevelReached"),
		"fast_track_coins":  getInt64(state, "MicroserieMaxCoins"),
		"banned":            isBanned,
		"ban_until":         banUntil,
	})
}

// ── Moderation helpers ─────────────────────────────────────────────────────

func botSecret() string {
	s := os.Getenv("BOT_SECRET")
	if s == "" {
		return "secret"
	}
	return s
}

func checkBotSecret(r *http.Request) bool {
	return r.URL.Query().Get("secret") == botSecret()
}

// patchLivesInState reads the player's current state blob, sets _lives to the
// given value, and persists it back. Returns the original _lives value.
func patchLivesInState(userID int64, newLives int64) (originalLives int64, err error) {
	raw, err := db.GetUserState(userID)
	if err != nil {
		return 0, err
	}
	state := map[string]interface{}{}
	if raw != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.UseNumber()
		if err := dec.Decode(&state); err != nil {
			return 0, err
		}
	}
	originalLives = getInt64(state, "_lives")
	// If _lives is 0 or missing in state, default to 5 so we don't restore
	// the player to 0 lives after verification or expiry.
	if originalLives <= 0 {
		originalLives = 5
	}
	state["_lives"] = newLives
	patched, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return originalLives, db.SaveUserState(userID, string(patched))
}

// restoreLivesInState sets _lives back to the original value without touching
// anything else. Used after verification completes or the code expires.
func restoreLivesInState(userID int64, originalLives int64) {
	raw, err := db.GetUserState(userID)
	if err != nil || raw == "" {
		return
	}
	state := map[string]interface{}{}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	if err := dec.Decode(&state); err != nil {
		return
	}
	state["_lives"] = originalLives
	patched, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = db.SaveUserState(userID, string(patched))
}

// startVerifExpirer runs in the background and restores _lives for codes that
// expire without the player completing verification.
// startCCUPoller writes a CCU snapshot every minute so we can track peak CCU
// over time without any external tooling. Old snapshots older than 30 days
// are pruned to keep the table from growing indefinitely.
func startCCUPoller() {
	go func() {
		for {
			time.Sleep(60 * time.Second)
			online, err := db.GetOnlinePlayers()
			if err != nil {
				log.Printf("CCUPoller: GetOnlinePlayers error: %v", err)
				continue
			}
			if err := db.RecordCCUSnapshot(online); err != nil {
				log.Printf("CCUPoller: RecordCCUSnapshot error: %v", err)
			}
			// Prune snapshots older than 30 days.
			cutoff := nowTs() - 30*86400
			db.db.Exec(`DELETE FROM ccu_snapshots WHERE ts < ?`, cutoff)
		}
	}()
}

func startVerifExpirer() {
	go func() {
		for {
			time.Sleep(15 * time.Second)
			now := time.Now()
			pendingVerifMu.Lock()
			for discordID, pv := range pendingVerifCodes {
				if now.After(pv.expiresAt) {
					delete(pendingVerifCodes, discordID)
					uid := pv.gameUID
					orig := pv.originalLives
					pendingVerifMu.Unlock()
					restoreLivesInState(uid, orig)
					log.Printf("VerifExpired: restored _lives=%d for user=%d discord=%s", orig, uid, discordID)
					// Force re-login so the game picks up the restored lives immediately.
					if err := db.InvalidateUserSessions(uid); err != nil {
						log.Printf("VerifExpired: InvalidateUserSessions error user=%d: %v", uid, err)
					} else {
						log.Printf("VerifExpired: invalidated sessions for user=%d (force re-login)", uid)
					}
					pendingVerifMu.Lock()
				}
			}
			pendingVerifMu.Unlock()
		}
	}()
}

// handleBotWhitelist — POST …/bot/whitelist?user_id=X&action=add|remove&secret=Y
func handleBotWhitelist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	action := r.URL.Query().Get("action")
	note := r.URL.Query().Get("note")
	switch action {
	case "add":
		if err := db.AddToWhitelist(uid, note); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Clear any pending suspicious alert for this user
		suspiciousAlertMu.Lock()
		delete(suspiciousAlertSent, uid)
		suspiciousAlertMu.Unlock()
		log.Printf("WHITELIST ADD user=%d note=%s", uid, note)
		jsonWrite(w, map[string]interface{}{"ok": true, "whitelisted": uid})
	case "remove":
		if err := db.RemoveFromWhitelist(uid); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		log.Printf("WHITELIST REMOVE user=%d", uid)
		jsonWrite(w, map[string]interface{}{"ok": true, "removed_from_whitelist": uid})
	default:
		jsonError(w, http.StatusBadRequest, "action must be 'add' or 'remove'")
	}
}

// handleBotBlacklist — POST …/bot/blacklist?device_uid=X&action=add|remove&reason=Y&secret=Z
// Blacklists a specific device UID (not user_id — device UIDs survive reinstalls).
// Use /bot/devices to look up a player's device UIDs first.
func handleBotBlacklist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	deviceUID := strings.TrimSpace(r.URL.Query().Get("device_uid"))
	if deviceUID == "" {
		jsonError(w, http.StatusBadRequest, "missing device_uid — use /bot/devices?user_id=X to find it")
		return
	}
	action := r.URL.Query().Get("action")
	reason := r.URL.Query().Get("reason")
	switch action {
	case "add":
		if err := db.AddToDeviceBlacklist(deviceUID, reason); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Kick all active sessions for every account that has used this device.
		// This boots the player immediately rather than waiting for their next packet.
		userIDs, _ := db.GetUserIDsByDeviceUID(deviceUID)
		for _, uid := range userIDs {
			if err := db.InvalidateUserSessions(uid); err != nil {
				log.Printf("BLACKLIST: InvalidateUserSessions error user=%d: %v", uid, err)
			}
		}
		log.Printf("BLACKLIST ADD device=%s reason=%q kicked %d user session(s)", deviceUID, reason, len(userIDs))
		jsonWrite(w, map[string]interface{}{"ok": true, "blacklisted_device": deviceUID, "sessions_killed": len(userIDs)})
	case "remove":
		if err := db.RemoveFromDeviceBlacklist(deviceUID); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		log.Printf("BLACKLIST REMOVE device=%s", deviceUID)
		jsonWrite(w, map[string]interface{}{"ok": true, "unblocked_device": deviceUID})
	default:
		jsonError(w, http.StatusBadRequest, "action must be 'add' or 'remove'")
	}
}

// handleBotDevices — GET …/bot/devices?user_id=X&secret=Y
// Returns all device UIDs a player has ever used, plus whether each is currently banned.
// Use this to find the device_uid you want to blacklist.
func handleBotDevices(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	devices, err := db.GetDevicesForUser(uid)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(devices))
	for _, d := range devices {
		out = append(out, map[string]interface{}{
			"device_uid":   d.DeviceUID,
			"device_os":    d.DeviceOS,
			"device_model": d.DeviceModel,
			"platform":     d.Platform,
			"last_seen":    d.UpdatedAt,
			"banned":       d.Banned,
		})
	}
	jsonWrite(w, map[string]interface{}{"ok": true, "user_id": uid, "devices": out})
}

// handleBotRegistered — GET …/bot/registered?page=1&secret=Y
// Returns paginated list of all Discord-linked accounts (for moderation overview).
func handleBotRegistered(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	page, _ := strconv.ParseInt(r.URL.Query().Get("page"), 10, 64)
	if page < 1 {
		page = 1
	}
	const perPage = 15
	offset := int((page - 1) * perPage)
	accounts, total, err := db.GetRegisteredAccounts(perPage, offset)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, map[string]interface{}{
			"discord_id":    a.DiscordID,
			"game_user_id":  a.GameUserID,
			"game_username": a.GameUsername,
			"linked_at":     a.LinkedAt,
		})
	}
	jsonWrite(w, map[string]interface{}{
		"ok":       true,
		"total":    total,
		"page":     page,
		"per_page": perPage,
		"accounts": out,
	})
}

// handleBotGenerateCode — POST …/bot/generate_code?user_id=X&discord_id=D&secret=Y
// Called by the Discord bot when a player runs /register <user_id>.
// Validates the account, checks the player has real progress (completed ≥1 level),
// generates a 6-digit verification code, writes it into the player's lives counter
// in-game, invalidates their session so they reload state immediately, and stores
// the code server-side for 5 minutes.
func handleBotGenerateCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	discordID := r.URL.Query().Get("discord_id")
	if discordID == "" {
		jsonError(w, http.StatusBadRequest, "missing_discord_id")
		return
	}

	// ── Rate limit: max 3 successful code requests per Discord user per hour ───
	// We check the count BEFORE validating anything, but only INCREMENT after all
	// checks pass and we're about to actually issue a code. This means failed
	// attempts (wrong ID, no progress, already linked) don't burn the user's quota.
	registerRateMu.Lock()
	now := time.Now()
	attempts := registerAttempts[discordID]
	var recent []time.Time
	for _, t := range attempts {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 3 {
		registerRateMu.Unlock()
		jsonWrite(w, map[string]interface{}{
			"ok":    false,
			"error": "rate_limited",
			"msg":   "You've requested too many codes in the last hour. Please wait before trying again.",
		})
		return
	}
	registerRateMu.Unlock()
	// NOTE: we add to recent only after all checks pass, right before issuing the code.

	// migration=true is passed by the bot's /login command (migrating to a new UID).
	// In that case we skip the "discord already linked" block — the caller already
	// has a linked account and is proving ownership of a NEW uid before migrating.
	isMigration := r.URL.Query().Get("migration") == "true"

	// ── Reject if this Discord user already has a fully linked account ───────
	// (skipped during migration — the bot's /login handler already verified the
	// Discord has a linked account and will call /bot/migrate after code confirm)
	existingUID, _ := db.GetGameUserByDiscord(discordID)
	if existingUID != 0 && !isMigration {
		jsonWrite(w, map[string]interface{}{
			"ok":             false,
			"error":          "discord_already_linked",
			"linked_user_id": existingUID,
			"msg":            "Your Discord is already linked to a game account. Use /login <new_user_id> to migrate to a new account instead.",
		})
		return
	}

	// ── Reject if this game account is already claimed by a different Discord ─
	existingDiscord, _ := db.GetDiscordByGameUser(uid)
	if existingDiscord != "" && existingDiscord != discordID {
		// During migration: the target UID is already registered to someone else.
		// Return a distinct error so the bot can give a clear message.
		if isMigration {
			jsonWrite(w, map[string]interface{}{
				"ok":    false,
				"error": "target_already_registered",
				"msg":   fmt.Sprintf("Game account `%d` is already registered to a different Discord user and cannot be migrated to.", uid),
			})
			return
		}
		jsonWrite(w, map[string]interface{}{
			"ok":    false,
			"error": "already_linked",
			"msg":   "That game account is already linked to a different Discord user. If this is your account, contact an admin.",
		})
		return
	}

	// ── Reject if game account does not exist ────────────────────────────────
	_, exists, _ := db.GetPlayerByID(uid)
	if !exists {
		jsonWrite(w, map[string]interface{}{
			"ok":    false,
			"error": "user_not_found",
			"msg":   fmt.Sprintf("Game account %d doesn't exist on the server yet. Complete at least the first level in Dragon Land, then try /register again.", uid),
		})
		return
	}

	// ── Reject if the player hasn't reached 12 campaign points ──────────────
	// Skip this check during migration (/login) — the new UID is a fresh install
	// and won't have any campaign score yet, which is expected.
	campaignScore, _ := db.GetCampaignScore("CAMPAIGN", uid)
	if campaignScore < 12 && !isMigration {
		jsonWrite(w, map[string]interface{}{
			"ok":    false,
			"error": "no_progress",
			"msg":   "You need to complete at least one level in Dragon Land before you can link your account. Play through the first stage and try again!",
		})
		return
	}

	// ── All checks passed — increment rate limit counter and issue code ─────
	registerRateMu.Lock()
	registerAttempts[discordID] = append(recent, now)
	registerRateMu.Unlock()

	// ── Generate 6-digit verification code ───────────────────────────────────
	// UnixNano() on Linux has microsecond resolution so the last 3 digits are
	// always 000 — codes would always end in 000. Use crypto/rand instead.
	var codeBuf [4]byte
	rand.Read(codeBuf[:])
	codeNum := (int(codeBuf[0])<<24|int(codeBuf[1])<<16|int(codeBuf[2])<<8|int(codeBuf[3]))&0x7fffffff%900000 + 100000
	code := fmt.Sprintf("%06d", codeNum)

	// ── Write the code into the player's _lives counter ──────────────────────
	// The player will see this number where their hearts normally appear.
	// We save originalLives so we can restore them when the code is consumed or expires.
	originalLives, patchErr := patchLivesInState(uid, mustParseInt64(code))
	if patchErr != nil {
		log.Printf("GenerateCode: patchLivesInState error user=%d: %v", uid, patchErr)
		originalLives = 5 // safe fallback — non-fatal
	}

	pendingVerifMu.Lock()
	pendingVerifCodes[discordID] = pendingVerif{
		code:          code,
		gameUID:       uid,
		expiresAt:     now.Add(5 * time.Minute),
		originalLives: originalLives,
	}
	pendingVerifMu.Unlock()

	// ── Send in-game inbox message to the player ──────────────────────────────
	db.SendInboxMessage(uid, 0, "InboxMessageSystemNews",
		map[string]interface{}{
			"sender":  "Dragon Land",
			"subject": "🔐 Discord Verification",
			"body":    "Your lives counter now shows a 6-digit verification code. Go to the main screen, look at your lives — then reply to the bot DM with that number to confirm your account link. The code expires in 5 minutes. Never share it with anyone.",
			"rewards": []interface{}{},
		},
	)

	// ── Invalidate session — forces the game to re-login and reload state ─────
	// This makes the lives counter update immediately without the player needing
	// to relaunch the game.
	if err := db.InvalidateUserSessions(uid); err != nil {
		log.Printf("GenerateCode: InvalidateUserSessions error user=%d: %v", uid, err)
	}

	name, _ := db.GetUserName(uid)
	log.Printf("GenerateCode: discord=%s → user=%d (%s) code=%s", discordID, uid, displayName(uid, name), code)

	// ── Audit log to Discord webhook ──────────────────────────────────────────
	discordEmbed(
		"🔐 Registration Attempt",
		fmt.Sprintf("Discord `%s` is linking to game account `%d` (%s).", discordID, uid, displayName(uid, name)),
		0x95a5a6,
		nil,
	)

	jsonWrite(w, map[string]interface{}{"ok": true, "expires_in": 300})
}

// handleBotVerifyCode — POST …/bot/verify_code?discord_id=D&code=XXXXXX&secret=Y
// Called by the bot when the player DMs the 6-digit code back.
// Validates the code, links the Discord ↔ game accounts, restores the player's
// real lives, and invalidates their session so the game reloads clean state.
func handleBotVerifyCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	discordID := r.URL.Query().Get("discord_id")
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	// migration=true is set by the bot's /login flow. In that case we only
	// validate ownership of the new UID — the actual Discord link and gem
	// bonus are handled by /bot/migrate after this call returns ok.
	isMigration := r.URL.Query().Get("migration") == "true"
	if discordID == "" || code == "" {
		jsonError(w, http.StatusBadRequest, "missing_params")
		return
	}

	pendingVerifMu.Lock()
	pending, exists := pendingVerifCodes[discordID]
	pendingVerifMu.Unlock()

	if !exists {
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "no_pending_code", "msg": "No active verification code found. Use /register first to start the process."})
		return
	}
	if time.Now().After(pending.expiresAt) {
		// Expired — restore lives, invalidate session, clean up.
		pendingVerifMu.Lock()
		delete(pendingVerifCodes, discordID)
		pendingVerifMu.Unlock()
		restoreLivesInState(pending.gameUID, pending.originalLives)
		// Invalidate session so the game reloads restored lives immediately
		// and can't sync the stale code value back in.
		if err := db.InvalidateUserSessions(pending.gameUID); err != nil {
			log.Printf("VerifyCode: InvalidateUserSessions expired uid=%d: %v", pending.gameUID, err)
		}
		log.Printf("VerifyCode: EXPIRED discord=%s uid=%d, lives restored, session invalidated", discordID, pending.gameUID)
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "code_expired", "msg": "That code has expired (codes are valid for 5 minutes). Use /register again to get a new one."})
		return
	}
	if code != pending.code {
		pending.attempts++
		const maxAttempts = 3
		if pending.attempts >= maxAttempts {
			// Max attempts reached — restore lives, wipe pending, invalidate session, lock them out.
			pendingVerifMu.Lock()
			delete(pendingVerifCodes, discordID)
			pendingVerifMu.Unlock()
			restoreLivesInState(pending.gameUID, pending.originalLives)
			// Invalidate session so the game is forced to re-login and reload
			// the restored lives — prevents a stale sync from writing the code
			// value back in after we've already cleaned up the pending entry.
			if err := db.InvalidateUserSessions(pending.gameUID); err != nil {
				log.Printf("VerifyCode: InvalidateUserSessions lockout uid=%d: %v", pending.gameUID, err)
			}
			log.Printf("VerifyCode: LOCKED OUT discord=%s uid=%d after %d failed attempts, lives restored, session invalidated", discordID, pending.gameUID, maxAttempts)
			jsonWrite(w, map[string]interface{}{"ok": false, "error": "max_attempts", "msg": "Too many wrong attempts. Your lives have been restored. Use /register again to get a new code."})
			return
		}
		// Save incremented attempt count back.
		pendingVerifMu.Lock()
		pendingVerifCodes[discordID] = pending
		pendingVerifMu.Unlock()
		remaining := maxAttempts - pending.attempts
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "wrong_code", "attempts_remaining": remaining, "msg": fmt.Sprintf("Wrong code. %d attempt(s) remaining before your lives are restored and the code is cancelled.", remaining)})
		return
	}
	// Code correct — consume it.
	pendingVerifMu.Lock()
	delete(pendingVerifCodes, discordID)
	pendingVerifMu.Unlock()

	// ── Restore the player's real lives ──────────────────────────────────────
	// Always done regardless of migration — the lives counter held the code.
	restoreLivesInState(pending.gameUID, pending.originalLives)

	// ── Invalidate session — forces the game to re-login and show real lives ──
	if err := db.InvalidateUserSessions(pending.gameUID); err != nil {
		log.Printf("VerifyCode: InvalidateUserSessions error user=%d: %v", pending.gameUID, err)
	}

	if isMigration {
		// Migration path: ownership of newUID is confirmed. The bot will call
		// /bot/migrate next, which does the actual Discord re-link and copies
		// all state. Do NOT link here and do NOT grant gems — the gem bonus
		// belongs on the old (source) account, not the fresh newUID stub.
		log.Printf("VerifyCode(migration): ownership confirmed discord=%s → newUID=%d", discordID, pending.gameUID)
		jsonWrite(w, map[string]interface{}{"ok": true})
		return
	}

	// ── Link the Discord ↔ game accounts ─────────────────────────────────────
	if err := db.LinkDiscord(discordID, pending.gameUID); err != nil {
		log.Printf("VerifyCode: LinkDiscord error discord=%s uid=%d: %v", discordID, pending.gameUID, err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// ── 10-gem Discord link bonus (once per game account) ─────────────────
	// Only granted if this game_user_id has never received it before.
	// If the account was banned and wiped, the bonus row is gone so a fresh
	// reinstall with a new game ID qualifies for another 10 gems — intentional.
	if !db.HasReceivedLinkBonus(pending.gameUID) {
		const bonusGems = 10
		stateRaw, stateErr := db.GetUserState(pending.gameUID)
		if stateErr == nil && stateRaw != "" {
			bonusState := map[string]interface{}{}
			dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
			dec.UseNumber()
			if dec.Decode(&bonusState) == nil {
				bonusState["_gems"] = getInt64(bonusState, "_gems") + bonusGems
				if patched, mErr := json.Marshal(bonusState); mErr == nil {
					if sErr := db.SaveUserState(pending.gameUID, string(patched)); sErr == nil {
						_ = db.MarkLinkBonusGiven(pending.gameUID)
						db.SendInboxMessage(pending.gameUID, 0, "InboxMessageSystemNews", map[string]interface{}{
							"sender":  "Dragon Land",
							"subject": "💎 Discord Link Reward!",
							"body":    fmt.Sprintf("Thanks for linking your Discord account! You've received %d gems as a welcome gift. Enjoy! 🐉", bonusGems),
							"rewards": []interface{}{},
						})
						log.Printf("VerifyCode: granted %d gem link bonus to user=%d", bonusGems, pending.gameUID)
					}
				}
			}
		}
	}

	name, _ := db.GetUserName(pending.gameUID)
	log.Printf("VerifyCode: LINKED discord=%s → user=%d (%s), restored _lives=%d", discordID, pending.gameUID, displayName(pending.gameUID, name), pending.originalLives)

	discordEmbed(
		"🔗 Account Linked",
		fmt.Sprintf("Discord user linked to game account `%d` (%s).", pending.gameUID, displayName(pending.gameUID, name)),
		0x2ecc71,
		nil,
	)

	jsonWrite(w, map[string]interface{}{
		"ok":           true,
		"linked_user":  pending.gameUID,
		"username":     displayName(pending.gameUID, name),
	})
}

// handleBotMigrate — POST …/bot/migrate?discord_id=D&new_user_id=X&secret=Y
// Called by the bot when a user runs /login <new_id> after reinstalling.
// Copies all data from the old linked account to the new user_id.
func handleBotMigrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	discordID := r.URL.Query().Get("discord_id")
	newUID, err := strconv.ParseInt(r.URL.Query().Get("new_user_id"), 10, 64)
	if err != nil || newUID == 0 || discordID == "" {
		jsonError(w, http.StatusBadRequest, "invalid_params")
		return
	}

	// Find old account linked to this Discord user.
	oldUID, err := db.GetGameUserByDiscord(discordID)
	if err != nil || oldUID == 0 {
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "not_linked", "msg": "Your Discord is not linked to any account. Use /register first."})
		return
	}
	if oldUID == newUID {
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "same_account", "msg": "That is already your linked account."})
		return
	}

	// The new user ID must already exist on the server (player must have opened
	// the game at least once after reinstalling). Without this, MigrateAccount
	// hits a FK constraint and the resulting DB error locks the server.
	_, newExists, _ := db.GetPlayerByID(newUID)
	if !newExists {
		jsonWrite(w, map[string]interface{}{
			"ok":    false,
			"error": "new_user_not_found",
			"msg":   "That user ID doesn't exist on the server yet. Complete at least the first level in Dragon Land on your new device, then run /login again.",
		})
		return
	}

	oldName, _ := db.GetUserName(oldUID)

	// Invalidate the OLD account's sessions BEFORE migrating.
	// This closes the race window where the old game client fires a sync command
	// after migration starts, which would overwrite the freshly copied state on
	// newUID. Any packet from oldUID after this point gets rejected as invalid_session.
	if err := db.InvalidateUserSessions(oldUID); err != nil {
		log.Printf("Migrate: InvalidateUserSessions oldUID=%d: %v", oldUID, err)
	}

	if err := db.MigrateAccount(oldUID, newUID); err != nil {
		log.Printf("Migrate error: discord=%s old=%d new=%d: %v", discordID, oldUID, newUID, err)
		// MigrateAccount may have partially re-linked Discord before failing.
		// Explicitly re-link back to oldUID so the player's Discord account
		// is never left pointing at the new (incomplete) UID.
		if relinkErr := db.LinkDiscord(discordID, oldUID); relinkErr != nil {
			log.Printf("Migrate: CRITICAL re-link rollback failed discord=%s oldUID=%d: %v", discordID, oldUID, relinkErr)
		} else {
			log.Printf("Migrate: rolled back Discord link to oldUID=%d after migrate failure", oldUID)
		}
		// Also restore the old account's sessions so the game can reconnect.
		_ = db.InvalidateUserSessions(oldUID)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Invalidate the NEW account's sessions so the game on the new device
	// is forced to re-login and load the migrated state immediately.
	if err := db.InvalidateUserSessions(newUID); err != nil {
		log.Printf("Migrate: InvalidateUserSessions newUID=%d: %v", newUID, err)
	}

	log.Printf("MIGRATE: discord=%s old=%d → new=%d", discordID, oldUID, newUID)
	discordEmbed(
		"🔄 Account Migrated",
		fmt.Sprintf("Account `%d` (%s) migrated to `%d`. Old account wiped.", oldUID, oldName, newUID),
		0x3498db,
		nil,
	)

	jsonWrite(w, map[string]interface{}{
		"ok":       true,
		"old_user": oldUID,
		"new_user": newUID,
	})
}

// handleBotLinkedAccount — GET …/bot/linked_account?discord_id=D&secret=Y
// Returns the game user_id linked to a Discord ID (used by the bot).
func handleBotLinkedAccount(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	discordID := r.URL.Query().Get("discord_id")
	if discordID == "" {
		jsonError(w, http.StatusBadRequest, "missing_discord_id")
		return
	}
	uid, err := db.GetGameUserByDiscord(discordID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if uid == 0 {
		jsonWrite(w, map[string]interface{}{"ok": true, "linked": false})
		return
	}
	name, _ := db.GetUserName(uid)
	jsonWrite(w, map[string]interface{}{
		"ok":      true,
		"linked":  true,
		"user_id": uid,
		"name":    displayName(uid, name),
	})
}

// handleBotIsRegistered — GET …/bot/is_registered?discord_id=D&secret=Y
// Owner-only lookup: given a Discord ID, returns the linked game account (if any)
// with full player stats. Used by the /isregistered bot command.
func handleBotIsRegistered(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	discordID := r.URL.Query().Get("discord_id")
	if discordID == "" {
		jsonError(w, http.StatusBadRequest, "missing_discord_id")
		return
	}
	uid, err := db.GetGameUserByDiscord(discordID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if uid == 0 {
		jsonWrite(w, map[string]interface{}{"ok": true, "registered": false})
		return
	}
	name, _ := db.GetUserName(uid)
	stateRaw, _ := db.GetUserState(uid)
	var coins, gems, maxLevel int64
	if stateRaw != "" {
		var stateMap map[string]interface{}
		dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
		dec.UseNumber()
		if dec.Decode(&stateMap) == nil {
			coins    = getInt64(stateMap, "_coins")
			gems     = getInt64(stateMap, "_gems")
			maxLevel = getInt64(stateMap, "_maxLevelCompleted")
		}
	}
	jsonWrite(w, map[string]interface{}{
		"ok":                true,
		"registered":        true,
		"game_user_id":      uid,
		"username":          displayName(uid, name),
		"coins":             coins,
		"gems":              gems,
		"max_level":         maxLevel,
	})
}

// handleBotFTRankUpdates — GET …/bot/ft_rank_updates?secret=Y
// Returns all Discord-linked players who have crossed into a higher Fast Track
// rank tier since they were last notified, then records the new tier for each
// so the same player is never returned twice for the same rank.
//
// Tier thresholds are passed as query params so the bot config stays the single
// source of truth:
//   tiers=bronze:500,silver:1000,gold:2000
// (comma-separated, ascending order required)
func handleBotFTRankUpdates(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Parse tiers from query string.
	tiersParam := r.URL.Query().Get("tiers")
	if tiersParam == "" {
		jsonError(w, http.StatusBadRequest, "missing_tiers_param")
		return
	}
	var tiers []FTRankTier
	for _, part := range strings.Split(tiersParam, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		score, err := strconv.ParseInt(kv[1], 10, 64)
		if err != nil || score < 0 {
			continue
		}
		// Sanitise the key: alphanumeric + underscore only, max 32 chars.
		// This value is written to the DB as rank_key — reject anything suspicious.
		key := kv[0]
		if len(key) == 0 || len(key) > 32 {
			continue
		}
		validKey := true
		for _, c := range key {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				validKey = false
				break
			}
		}
		if !validKey {
			continue
		}
		tiers = append(tiers, FTRankTier{Key: key, MinScore: score})
	}
	if len(tiers) == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_tiers_param")
		return
	}

	upgrades, err := db.GetFTRankUpgrades(tiers)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Persist the new tier for each upgraded player immediately so the next poll
	// won't return them again.
	for _, u := range upgrades {
		if err := db.SetFTRank(u.GameUserID, u.NewRankKey); err != nil {
			log.Printf("ft_rank_updates: SetFTRank user=%d rank=%s err=%v", u.GameUserID, u.NewRankKey, err)
		}
	}

	out := make([]map[string]interface{}, 0, len(upgrades))
	for _, u := range upgrades {
		out = append(out, map[string]interface{}{
			"discord_id":   u.DiscordID,
			"game_user_id": u.GameUserID,
			"username":     displayName(u.GameUserID, u.Username),
			"score":        u.Score,
			"new_rank_key": u.NewRankKey,
		})
	}
	jsonWrite(w, map[string]interface{}{
		"ok":       true,
		"upgrades": out,
		"count":    len(out),
	})
}


// handleBotDiscordMap — GET …/bot/discord_map?ids=1001,1002,1003&secret=Y
// Batch lookup: given a comma-separated list of game user IDs, returns a map
// of game_user_id → discord_id for any that have a linked Discord account.
// IDs with no link are simply omitted from the response.
// Used by the bot to annotate leaderboard rows with clickable Discord profiles.
func handleBotDiscordMap(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	idsParam := strings.TrimSpace(r.URL.Query().Get("ids"))
	if idsParam == "" {
		jsonWrite(w, map[string]interface{}{"ok": true, "map": map[string]string{}})
		return
	}
	parts := strings.Split(idsParam, ",")
	result := make(map[string]string, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		uid, err := strconv.ParseInt(p, 10, 64)
		if err != nil || uid == 0 {
			continue
		}
		did, err := db.GetDiscordByGameUser(uid)
		if err != nil || did == "" {
			continue
		}
		result[p] = did
	}
	jsonWrite(w, map[string]interface{}{"ok": true, "map": result})
}

// handleBotGift — POST …/bot/gift?user_id=X&type=coins|gems|lives&amount=N&secret=Y
// Adds coins, gems, or lives directly to a player's state blob.
// Sends an in-game inbox notification so the player knows what they received.
// Lives are capped at 99 to avoid triggering the suspicious-activity detector.
func handleBotGift(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}

	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	giftType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	amount, err := strconv.ParseInt(r.URL.Query().Get("amount"), 10, 64)
	if err != nil || amount <= 0 {
		jsonError(w, http.StatusBadRequest, "invalid_amount — must be a positive integer")
		return
	}
	if giftType != "coins" && giftType != "gems" && giftType != "lives" {
		jsonError(w, http.StatusBadRequest, "invalid_type — must be coins, gems, or lives")
		return
	}

	// Verify the player exists before touching their state.
	name, exists, _ := db.GetPlayerByID(uid)
	if !exists {
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "user_not_found"})
		return
	}

	// Load current state blob.
	stateRaw, err := db.GetUserState(uid)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to read player state")
		return
	}
	state := map[string]interface{}{}
	if stateRaw != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
		dec.UseNumber()
		if err := dec.Decode(&state); err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to parse player state")
			return
		}
	}

	// Map gift type → state field key and display label.
	stateKey := map[string]string{
		"coins": "_coins",
		"gems":  "_gems",
		"lives": "_lives",
	}[giftType]
	label := map[string]string{
		"coins": "Coins",
		"gems":  "Gems",
		"lives": "Lives",
	}[giftType]
	emoji := map[string]string{
		"coins": "💰",
		"gems":  "💎",
		"lives": "❤️",
	}[giftType]

	before := getInt64(state, stateKey)
	after := before + amount

	// Cap lives at 99 — values above 9999 trigger the suspicious-activity detector.
	if giftType == "lives" && after > 99 {
		after = 99
	}
	state[stateKey] = after

	// Persist updated state.
	patched, err := json.Marshal(state)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to marshal state")
		return
	}
	if err := db.SaveUserState(uid, string(patched)); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Force re-login so the game picks up the new values immediately.
	if err := db.InvalidateUserSessions(uid); err != nil {
		log.Printf("Gift: InvalidateUserSessions error user=%d: %v", uid, err)
	}

	// Send in-game inbox notification.
	actualGifted := after - before
	db.SendInboxMessage(uid, 0, "InboxMessageSystemNews", map[string]interface{}{
		"sender":  "Dragon Land",
		"subject": fmt.Sprintf("%s You received a gift!", emoji),
		"body":    fmt.Sprintf("An admin has gifted you %d %s! Enjoy!", actualGifted, label),
		"rewards": []interface{}{},
	})

	dn := displayName(uid, name)
	log.Printf("GIFT: user=%d (%s) type=%s amount=%d (before=%d after=%d)", uid, dn, giftType, actualGifted, before, after)

	discordEmbed(
		fmt.Sprintf("%s Admin Gift Sent", emoji),
		fmt.Sprintf("Player **%s** (`%d`) received **%d %s** from an admin.", dn, uid, actualGifted, label),
		map[string]int{"coins": 0xf1c40f, "gems": 0x9b59b6, "lives": 0xe74c3c}[giftType],
		[]map[string]interface{}{
			{"name": "Before", "value": fmt.Sprintf("%d", before), "inline": true},
			{"name": "After", "value": fmt.Sprintf("%d", after), "inline": true},
		},
	)

	jsonWrite(w, map[string]interface{}{
		"ok":       true,
		"user_id":  uid,
		"username": dn,
		"type":     giftType,
		"gifted":   actualGifted,
		"before":   before,
		"after":    after,
	})
}

// handleBotBanSession — POST .../bot/ban_session?user_id=X&duration_seconds=N&reason=R&secret=Y
// Soft ban: account data is preserved but the player is kicked, hidden from leaderboards,
// and blocked from logging in until the ban expires (duration_seconds=0 = permanent).
func handleBotBanSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	durationSeconds, _ := strconv.ParseInt(r.URL.Query().Get("duration_seconds"), 10, 64)
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "No reason provided."
	}
	name, exists, _ := db.GetPlayerByID(uid)
	if !exists {
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "user_not_found"})
		return
	}
	var banUntil int64
	if durationSeconds > 0 {
		banUntil = time.Now().Unix() + durationSeconds
	}
	if err := db.BanUser(uid, reason, banUntil); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = db.InvalidateUserSessions(uid)
	discordID, _ := db.GetDiscordByGameUser(uid)
	dn := displayName(uid, name)
	log.Printf("BAN_SESSION: user=%d (%s) reason=%q ban_until=%d", uid, dn, reason, banUntil)
	banUntilStr := "permanent"
	if banUntil > 0 {
		banUntilStr = fmt.Sprintf("until %d", banUntil)
	}
	discordEmbed(
		"\U0001f528 Player Soft-Banned",
		fmt.Sprintf("Player **%s** (`%d`) has been banned (%s). Account data preserved.", dn, uid, banUntilStr),
		0xe74c3c,
		[]map[string]interface{}{{"name": "Reason", "value": reason, "inline": false}},
	)
	jsonWrite(w, map[string]interface{}{
		"ok":         true,
		"user_id":    uid,
		"username":   dn,
		"ban_until":  banUntil,
		"discord_id": discordID,
	})
}

// handleBotUnban — POST .../bot/unban?user_id=X&secret=Y
// Lifts a soft ban so the player can log in again.
func handleBotUnban(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	name, exists, _ := db.GetPlayerByID(uid)
	if !exists {
		jsonWrite(w, map[string]interface{}{"ok": false, "error": "user_not_found"})
		return
	}
	if err := db.UnbanUser(uid); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	discordID, _ := db.GetDiscordByGameUser(uid)
	dn := displayName(uid, name)
	log.Printf("UNBAN: user=%d (%s)", uid, dn)
	discordEmbed("\u2705 Player Unbanned", fmt.Sprintf("Player **%s** (`%d`) has been unbanned.", dn, uid), 0x2ecc71, nil)
	jsonWrite(w, map[string]interface{}{
		"ok":         true,
		"user_id":    uid,
		"username":   dn,
		"discord_id": discordID,
	})
}

// handleBotUnlink — POST .../bot/unlink?user_id=X&secret=Y
// Removes the Discord <-> game account link for the given game user_id.
func handleBotUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	name, _, _ := db.GetPlayerByID(uid)
	discordID, _ := db.GetDiscordByGameUser(uid)
	if err := db.UnlinkDiscord(uid); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dn := displayName(uid, name)
	log.Printf("UNLINK: user=%d (%s) was linked to discord=%s", uid, dn, discordID)
	discordEmbed("\U0001f517 Discord Unlinked", fmt.Sprintf("Game account `%d` (%s) unlinked from Discord `%s`.", uid, dn, discordID), 0xe67e22, nil)
	jsonWrite(w, map[string]interface{}{
		"ok":         true,
		"user_id":    uid,
		"username":   dn,
		"discord_id": discordID,
	})
}

// handleBotLinkedAccountByGameID — GET .../bot/linked_account_by_game_id?user_id=X&secret=Y
// Reverse lookup: given a game user_id, returns the linked Discord ID (if any).
func handleBotLinkedAccountByGameID(w http.ResponseWriter, r *http.Request) {
	if !checkBotSecret(r) {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uid, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	name, _, _ := db.GetPlayerByID(uid)
	discordID, _ := db.GetDiscordByGameUser(uid)
	if discordID == "" {
		jsonWrite(w, map[string]interface{}{"ok": true, "linked": false, "name": displayName(uid, name)})
		return
	}
	jsonWrite(w, map[string]interface{}{
		"ok":         true,
		"linked":     true,
		"discord_id": discordID,
		"name":       displayName(uid, name),
	})
}

// Permanently deletes all data for a user. Protected by a secret token.
func handleBotBan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	secret := os.Getenv("BOT_SECRET")
	if secret == "" {
		secret = "secret" // default — set BOT_SECRET env var to override
	}
	if r.URL.Query().Get("secret") != secret {
		jsonError(w, http.StatusForbidden, "forbidden")
		return
	}
	uidStr := r.URL.Query().Get("user_id")
	uid, err := strconv.ParseInt(uidStr, 10, 64)
	if err != nil || uid == 0 {
		jsonError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}
	name, _ := db.GetUserName(uid)
	// Fetch the Discord link BEFORE wiping so we can return it to the bot for DM notification.
	discordID, _ := db.GetDiscordByGameUser(uid)
	// Kick active sessions first so the player is booted immediately,
	// then remove the Discord link, then wipe all data.
	_ = db.InvalidateUserSessions(uid)
	_ = db.UnlinkDiscord(uid)
	if err := db.DeleteUserData(uid); err != nil {
		log.Printf("BAN error user=%d: %v", uid, err)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("BAN: user=%d name=%s wiped + sessions killed + discord unlinked", uid, name)
	discordEmbed(
		"🔨 Player Banned",
		fmt.Sprintf("All data for player `%d` (%s) has been permanently deleted.", uid, name),
		0xe74c3c,
		nil,
	)
	jsonWrite(w, map[string]interface{}{"ok": true, "deleted_user_id": uid, "discord_id": discordID, "username": name})
}

// handleBotStats — GET …/bot/server_stats
func handleBotStats(w http.ResponseWriter, r *http.Request) {
	total, err1 := db.GetTotalUsers()
	online, err2 := db.GetOnlinePlayers()
	newToday, err3 := db.GetNewUsersToday()
	peakCCU, err4 := db.GetPeakCCU()
	peakCCU24h, err5 := db.GetPeakCCU24h()
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		jsonError(w, http.StatusInternalServerError, "db_error")
		return
	}
	jsonWrite(w, map[string]interface{}{
		"ok":           true,
		"total":        total,
		"online":       online,
		"new_today":    newToday,
		"peak_ccu":     peakCCU,
		"peak_ccu_24h": peakCCU24h,
		"timestamp":    time.Now().Unix(),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, map[string]interface{}{"status": "ok", "timestamp": time.Now().Unix()})
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, map[string]interface{}{"name": "Dragon Land Game Backend", "version": "3.1.0"})
}

// ── Config loader ──────────────────────────────────────────────────────────

func loadGameConfig(execDir string) {
	path := filepath.Join(execDir, "gamedata.json")
	data, err := readFile(path)
	if err != nil {
		log.Printf("WARNING: gamedata.json not found at %s — config will be empty", path)
		gameConfig = json.RawMessage("{}")
		return
	}
	gameConfig = json.RawMessage(data)
	log.Printf("Loaded gamedata.json (%d bytes)", len(data))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	var err error
	dbPath := resolveDatabasePath()
	log.Printf("Opening database: %s", dbPath)
	db, err = NewDatabase(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	startVerifExpirer()
	startCCUPoller()

	// Read webhook URL at runtime so it works with export, start.sh, or systemd.
	discordWebhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	if discordWebhookURL == "" {
		log.Printf("WARNING: DISCORD_WEBHOOK_URL is not set — suspicious-activity alerts and Discord notifications are DISABLED.")
		log.Printf("         Add your webhook to start.sh and run ./start.sh instead of ./game_server directly.")
	} else {
		log.Printf("Discord webhook configured — notifications enabled.")
	}

	execDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	loadGameConfig(execDir)

	mux := http.NewServeMux()

	withCORS := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Content-Encoding")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}

	apiHandler := withCORS(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/user/login"):
			handleLogin(w, r)
		case strings.HasSuffix(path, "/packet"):
			handlePacket(w, r)
		case strings.HasSuffix(path, "/unauthorized/track"):
			handleUnauthorizedTrack(w, r)
		case strings.HasSuffix(path, "/track"):
			handleTrack(w, r)
		case strings.Contains(path, "/query/"):
			handleQuery(w, r)
		case strings.Contains(path, "/bot/server_stats"):
			handleBotStats(w, r)
		case strings.Contains(path, "/bot/player_stats"):
			handleBotPlayerStats(w, r)
		case strings.Contains(path, "/bot/search_player"):
			handleBotSearchPlayer(w, r)
		case strings.Contains(path, "/bot/whitelist"):
			handleBotWhitelist(w, r)
		case strings.Contains(path, "/bot/blacklist"):
			handleBotBlacklist(w, r)
		case strings.Contains(path, "/bot/devices"):
			handleBotDevices(w, r)
		case strings.Contains(path, "/bot/is_registered"):
			handleBotIsRegistered(w, r)
		case strings.Contains(path, "/bot/registered"):
			handleBotRegistered(w, r)
		case strings.Contains(path, "/bot/generate_code"):
			handleBotGenerateCode(w, r)
		case strings.Contains(path, "/bot/verify_code"):
			handleBotVerifyCode(w, r)
		case strings.Contains(path, "/bot/migrate"):
			handleBotMigrate(w, r)
		case strings.Contains(path, "/bot/linked_account_by_game_id"):
			handleBotLinkedAccountByGameID(w, r)
		case strings.Contains(path, "/bot/linked_account"):
			handleBotLinkedAccount(w, r)
		case strings.Contains(path, "/bot/ft_rank_updates"):
			handleBotFTRankUpdates(w, r)
		case strings.Contains(path, "/bot/discord_map"):
			handleBotDiscordMap(w, r)
		case strings.Contains(path, "/bot/gift"):
			handleBotGift(w, r)
		case strings.Contains(path, "/bot/ban_session"):
			handleBotBanSession(w, r)
		case strings.Contains(path, "/bot/unban"):
			handleBotUnban(w, r)
		case strings.Contains(path, "/bot/unlink"):
			handleBotUnlink(w, r)
		case strings.Contains(path, "/bot/ban"):
			handleBotBan(w, r)
		case strings.HasSuffix(path, "/exceptions"):
			jsonWrite(w, map[string]interface{}{"ok": true})
		case strings.HasSuffix(path, "/crash"):
			jsonWrite(w, map[string]interface{}{"ok": true})
		default:
			log.Printf("Unknown path: %s %s", r.Method, path)
			jsonWrite(w, map[string]interface{}{"ok": true})
		}
	})

	mux.HandleFunc("/api/v3/", apiHandler)
	mux.HandleFunc("/RHJhZ29uTGFuZA/api/v3/", apiHandler)
	mux.HandleFunc("/health", withCORS(handleHealth))
	mux.HandleFunc("/", withCORS(handleRoot))

	addr := strings.TrimSpace(os.Getenv("DRAGONLAND_LISTEN_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:5192"
	}
	log.Printf("Dragon Land Game Backend v3.1.0 — listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
