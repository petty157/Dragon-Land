package main

import (
	"database/sql"
	"log"
	"time"
)

// ── Moderation schema ──────────────────────────────────────────────────────
// Drop this file into your project alongside database.go and main.go.
// Then add ONE call in database.go's NewDatabase() — see PATCHES.md.

func (d *Database) initModerationSchema() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS whitelist (
			user_id  INTEGER PRIMARY KEY,
			note     TEXT    NOT NULL DEFAULT '',
			added_at INTEGER NOT NULL
		);

		-- Blacklist blocks login by user_id OR device_uid (both stored on one row).
		CREATE TABLE IF NOT EXISTS blacklist (
			user_id    INTEGER PRIMARY KEY,
			device_uid TEXT    NOT NULL DEFAULT '',
			reason     TEXT    NOT NULL DEFAULT '',
			added_at   INTEGER NOT NULL
		);

		-- Discord ↔ game account link for account recovery after reinstall.
		CREATE TABLE IF NOT EXISTS discord_links (
			discord_id   TEXT    PRIMARY KEY,
			game_user_id INTEGER NOT NULL UNIQUE,
			linked_at    INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_discord_links_game ON discord_links(game_user_id);

		-- Tracks the last Fast Track rank tier we notified each player about.
		-- rank_key: "bronze" | "silver" | "gold" (or future tiers).
		-- Updated every time the bot fires a rank-up webhook so we never double-ping.
		CREATE TABLE IF NOT EXISTS fast_track_ranks (
			game_user_id INTEGER PRIMARY KEY,
			rank_key     TEXT    NOT NULL DEFAULT '',
			notified_at  INTEGER NOT NULL
		);

		-- Soft bans: account data is preserved but the player is hidden from
		-- leaderboards and blocked from logging in until the ban expires.
		-- ban_until = 0 means permanent.
		CREATE TABLE IF NOT EXISTS bans (
			user_id    INTEGER PRIMARY KEY,
			reason     TEXT    NOT NULL DEFAULT '',
			ban_until  INTEGER NOT NULL DEFAULT 0,
			banned_at  INTEGER NOT NULL
		);
	`)
	return err
}

// ── Soft ban ───────────────────────────────────────────────────────────────

// BanUser upserts a ban for the given user.
// banUntil = 0 means permanent; anything else is a Unix timestamp when the ban lifts.
func (d *Database) BanUser(userID int64, reason string, banUntil int64) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO bans(user_id, reason, ban_until, banned_at) VALUES(?,?,?,?)`,
		userID, reason, banUntil, nowTs(),
	)
	return err
}

// UnbanUser removes a ban for the given user.
func (d *Database) UnbanUser(userID int64) error {
	_, err := d.db.Exec(`DELETE FROM bans WHERE user_id=?`, userID)
	return err
}

// IsBanned returns true if an active ban exists for the user. Auto-cleans expired bans.
func (d *Database) IsBanned(userID int64) bool {
	var banUntil int64
	err := d.db.QueryRow(`SELECT ban_until FROM bans WHERE user_id=?`, userID).Scan(&banUntil)
	if err != nil {
		return false
	}
	if banUntil == 0 {
		return true // permanent
	}
	if time.Now().Unix() < banUntil {
		return true // still active
	}
	// Expired — clean it up silently
	d.db.Exec(`DELETE FROM bans WHERE user_id=?`, userID)
	return false
}

// GetBanInfo returns (reason, banUntil, found). banUntil=0 means permanent.
// Returns found=false if no active ban exists.
func (d *Database) GetBanInfo(userID int64) (reason string, banUntil int64, found bool) {
	err := d.db.QueryRow(`SELECT reason, ban_until FROM bans WHERE user_id=?`, userID).Scan(&reason, &banUntil)
	if err != nil {
		return "", 0, false
	}
	if banUntil != 0 && time.Now().Unix() >= banUntil {
		d.db.Exec(`DELETE FROM bans WHERE user_id=?`, userID)
		return "", 0, false
	}
	return reason, banUntil, true
}

// ── Fast Track rank tracking ───────────────────────────────────────────────

// GetFTRank returns the last notified Fast Track rank key for a player (empty if none).
func (d *Database) GetFTRank(gameUserID int64) (string, error) {
	var rk string
	err := d.db.QueryRow(
		`SELECT rank_key FROM fast_track_ranks WHERE game_user_id=?`, gameUserID,
	).Scan(&rk)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return rk, err
}

// SetFTRank upserts the last notified Fast Track rank key for a player.
func (d *Database) SetFTRank(gameUserID int64, rankKey string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO fast_track_ranks(game_user_id, rank_key, notified_at) VALUES(?,?,?)`,
		gameUserID, rankKey, nowTs(),
	)
	return err
}

// FTRankTier is one rank tier definition passed from the bot config.
type FTRankTier struct {
	Key      string
	MinScore int64
}

// FTRankUpgrade is one player who just crossed into a higher tier.
type FTRankUpgrade struct {
	DiscordID  string
	GameUserID int64
	Username   string
	Score      int64
	NewRankKey string
}

// GetFTRankUpgrades returns all Discord-linked players whose current FAST_TRACK
// leaderboard score qualifies for a higher tier than they were last notified for.
// tiers must be sorted ascending by MinScore.
func (d *Database) GetFTRankUpgrades(tiers []FTRankTier) ([]FTRankUpgrade, error) {
	if len(tiers) == 0 {
		return nil, nil
	}

	// Fetch all Discord-linked players with their current FAST_TRACK score.
	// Collect into memory first to avoid holding a rows cursor while calling
	// GetFTRank (which would deadlock on the single SQLite connection).
	rows, err := d.db.Query(`
		SELECT dl.discord_id, dl.game_user_id, COALESCE(u.username,''), COALESCE(cl.score, 0)
		FROM discord_links dl
		LEFT JOIN users u ON u.id = dl.game_user_id
		LEFT JOIN campaign_leaderboard cl
			ON cl.leaderboard_id = 'FAST_TRACK' AND cl.user_id = dl.game_user_id
	`)
	if err != nil {
		return nil, err
	}
	type playerRow struct {
		discordID string
		gameUID   int64
		username  string
		score     int64
	}
	var players []playerRow
	for rows.Next() {
		var p playerRow
		rows.Scan(&p.discordID, &p.gameUID, &p.username, &p.score)
		players = append(players, p)
	}
	rows.Close() // release connection before any further Exec/QueryRow

	var upgrades []FTRankUpgrade
	for _, p := range players {
		// Highest tier this score qualifies for.
		newRank := ""
		for _, t := range tiers {
			if p.score >= t.MinScore {
				newRank = t.Key
			}
		}
		if newRank == "" {
			continue // below all tiers
		}
		lastRank, _ := d.GetFTRank(p.gameUID)
		if lastRank == newRank {
			continue // already notified
		}
		// Only upgrade — never downgrade.
		lastIdx, newIdx := -1, -1
		for i, t := range tiers {
			if t.Key == lastRank {
				lastIdx = i
			}
			if t.Key == newRank {
				newIdx = i
			}
		}
		if newIdx <= lastIdx {
			continue
		}
		upgrades = append(upgrades, FTRankUpgrade{
			DiscordID:  p.discordID,
			GameUserID: p.gameUID,
			Username:   p.username,
			Score:      p.score,
			NewRankKey: newRank,
		})
	}
	return upgrades, nil
}

// ── Whitelist ──────────────────────────────────────────────────────────────

// IsWhitelisted returns true if the user is in the whitelist.
// Whitelisted players bypass suspicious-activity checks.
func (d *Database) IsWhitelisted(userID int64) bool {
	var c int
	d.db.QueryRow(`SELECT COUNT(*) FROM whitelist WHERE user_id=?`, userID).Scan(&c)
	return c > 0
}

func (d *Database) AddToWhitelist(userID int64, note string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO whitelist(user_id, note, added_at) VALUES(?,?,?)`,
		userID, note, nowTs(),
	)
	return err
}

func (d *Database) RemoveFromWhitelist(userID int64) error {
	_, err := d.db.Exec(`DELETE FROM whitelist WHERE user_id=?`, userID)
	return err
}

// ── Blacklist ──────────────────────────────────────────────────────────────

// IsBlacklisted returns true if the user_id or device_uid is banned.
func (d *Database) IsBlacklisted(userID int64, deviceUID string) bool {
	var c int
	if deviceUID != "" {
		d.db.QueryRow(
			`SELECT COUNT(*) FROM blacklist WHERE user_id=? OR (device_uid != '' AND device_uid=?)`,
			userID, deviceUID,
		).Scan(&c)
	} else {
		d.db.QueryRow(`SELECT COUNT(*) FROM blacklist WHERE user_id=?`, userID).Scan(&c)
	}
	return c > 0
}

func (d *Database) AddToBlacklist(userID int64, deviceUID, reason string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO blacklist(user_id, device_uid, reason, added_at) VALUES(?,?,?,?)`,
		userID, deviceUID, reason, nowTs(),
	)
	return err
}

func (d *Database) RemoveFromBlacklist(userID int64) error {
	_, err := d.db.Exec(`DELETE FROM blacklist WHERE user_id=?`, userID)
	return err
}

// ── Discord links ──────────────────────────────────────────────────────────

// LinkDiscord links a Discord user ID to a game user ID (upserts).
func (d *Database) LinkDiscord(discordID string, gameUserID int64) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO discord_links(discord_id, game_user_id, linked_at) VALUES(?,?,?)`,
		discordID, gameUserID, nowTs(),
	)
	return err
}

// GetGameUserByDiscord returns the game user_id linked to a Discord ID (0 if none).
func (d *Database) GetGameUserByDiscord(discordID string) (int64, error) {
	var uid int64
	err := d.db.QueryRow(
		`SELECT game_user_id FROM discord_links WHERE discord_id=?`, discordID,
	).Scan(&uid)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return uid, err
}

// GetDiscordByGameUser returns the Discord ID linked to a game user (empty if none).
func (d *Database) GetDiscordByGameUser(gameUserID int64) (string, error) {
	var did string
	err := d.db.QueryRow(
		`SELECT discord_id FROM discord_links WHERE game_user_id=?`, gameUserID,
	).Scan(&did)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return did, err
}

// ── Device blacklist ───────────────────────────────────────────────────────
// Separate from the user blacklist: bans a device_uid regardless of which
// account it logs in with. The devices table gains a `banned` column via
// initModerationSchema (ALTER TABLE … ADD COLUMN IF NOT EXISTS is idempotent).

// IsBlacklistedDevice returns true if the given device_uid is device-banned.
// Call this at login when you only have the device UID (before user lookup).
func (d *Database) IsBlacklistedDevice(deviceUID string) bool {
	if deviceUID == "" {
		return false
	}
	var c int
	d.db.QueryRow(
		`SELECT COUNT(*) FROM blacklist WHERE device_uid != '' AND device_uid=?`, deviceUID,
	).Scan(&c)
	return c > 0
}

// AddToDeviceBlacklist bans a device_uid.  If a blacklist row already exists
// for this device it is replaced (upsert on device_uid).
func (d *Database) AddToDeviceBlacklist(deviceUID, reason string) error {
	// We store device-only bans with user_id = 0 so the existing schema works.
	_, err := d.db.Exec(
		`INSERT INTO blacklist(user_id, device_uid, reason, added_at)
		 VALUES(0, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		     device_uid = excluded.device_uid,
		     reason     = excluded.reason,
		     added_at   = excluded.added_at`,
		deviceUID, reason, nowTs(),
	)
	if err != nil {
		// user_id=0 conflict already exists — do a direct update by device_uid.
		_, err = d.db.Exec(
			`INSERT OR REPLACE INTO blacklist(user_id, device_uid, reason, added_at)
			 SELECT user_id, ?, ?, ? FROM blacklist WHERE device_uid=?`,
			deviceUID, reason, nowTs(), deviceUID,
		)
	}
	return err
}

// RemoveFromDeviceBlacklist lifts a device ban by device_uid.
func (d *Database) RemoveFromDeviceBlacklist(deviceUID string) error {
	_, err := d.db.Exec(`DELETE FROM blacklist WHERE device_uid=?`, deviceUID)
	return err
}

// ── Device listing ─────────────────────────────────────────────────────────

// DeviceInfo holds one row from the devices table enriched with ban status.
type DeviceInfo struct {
	DeviceUID   string
	DeviceOS    string
	DeviceModel string
	Platform    string
	UpdatedAt   int64
	Banned      bool
}

// GetDevicesForUser returns all device records for a user, each annotated
// with whether that device_uid is currently in the blacklist.
func (d *Database) GetDevicesForUser(userID int64) ([]DeviceInfo, error) {
	rows, err := d.db.Query(`
		SELECT dv.device_uid, dv.device_os, dv.device_model, dv.platform, dv.updated_at,
		       CASE WHEN bl.device_uid IS NOT NULL THEN 1 ELSE 0 END AS banned
		FROM devices dv
		LEFT JOIN blacklist bl ON bl.device_uid = dv.device_uid AND bl.device_uid != ''
		WHERE dv.user_id = ?
		ORDER BY dv.updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceInfo
	for rows.Next() {
		var di DeviceInfo
		var banned int
		rows.Scan(&di.DeviceUID, &di.DeviceOS, &di.DeviceModel, &di.Platform, &di.UpdatedAt, &banned)
		di.Banned = banned == 1
		out = append(out, di)
	}
	return out, nil
}

// ── Registered accounts (Discord-linked) ──────────────────────────────────

// RegisteredAccount is one row returned by GetRegisteredAccounts.
type RegisteredAccount struct {
	DiscordID    string
	GameUserID   int64
	GameUsername string
	LinkedAt     int64
}

// GetRegisteredAccounts returns a paginated list of all Discord-linked accounts
// joined with the users table to include the in-game username.
func (d *Database) GetRegisteredAccounts(limit, offset int) ([]RegisteredAccount, int64, error) {
	var total int64
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM discord_links`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.db.Query(`
		SELECT dl.discord_id, dl.game_user_id, COALESCE(u.username,''), dl.linked_at
		FROM discord_links dl
		LEFT JOIN users u ON u.id = dl.game_user_id
		ORDER BY dl.linked_at DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []RegisteredAccount
	for rows.Next() {
		var a RegisteredAccount
		rows.Scan(&a.DiscordID, &a.GameUserID, &a.GameUsername, &a.LinkedAt)
		out = append(out, a)
	}
	return out, total, nil
}

// MigrateAccount copies all data from oldUID → newUID (account recovery after reinstall).
// Leaderboard scores are merged keeping the highest value.
// The old account is fully deleted after migration.
func (d *Database) MigrateAccount(oldUID, newUID int64) error {
	// Ensure the destination user row exists before writing any child rows.
	// user_progress has FK → users.id, so SaveUserState crashes with a constraint
	// error if newUID was never inserted (fresh reinstall = new game-assigned ID).
	// EnsureUser is idempotent — safe to call even if the user already exists.
	if _, err := d.EnsureUser(newUID); err != nil {
		log.Printf("MigrateAccount: EnsureUser newUID=%d: %v", newUID, err)
		return err
	}

	// Copy game state blob. Abort if this fails — never wipe the old account
	// without having successfully written its data to the new one first.
	if state, err := d.GetUserState(oldUID); err == nil && state != "" {
		if err2 := d.SaveUserState(newUID, state); err2 != nil {
			log.Printf("MigrateAccount: SaveUserState newUID=%d: %v", newUID, err2)
			return err2
		}
	}

	// Copy display name
	if name, err := d.GetUserName(oldUID); err == nil && name != "" {
		_ = d.SetUserName(newUID, name)
	}

	// Copy leaderboard scores — keep the higher value per board.
	// IMPORTANT: collect all rows into memory BEFORE closing, then do the
	// inserts separately. The DB uses SetMaxOpenConns(1), so running Exec
	// inside a rows.Next() loop deadlocks — both calls fight for the one
	// connection that rows is already holding.
	type lbRow struct {
		lbID  string
		score int64
	}
	var lbRows []lbRow
	if rows, err := d.db.Query(
		`SELECT leaderboard_id, score FROM campaign_leaderboard WHERE user_id=?`, oldUID,
	); err == nil {
		for rows.Next() {
			var r lbRow
			if rows.Scan(&r.lbID, &r.score) == nil {
				lbRows = append(lbRows, r)
			}
		}
		rows.Close() // close BEFORE any Exec — releases the single connection
	}
	for _, r := range lbRows {
		d.db.Exec(`
			INSERT INTO campaign_leaderboard(leaderboard_id, user_id, score, updated_at)
			VALUES(?,?,?,?)
			ON CONFLICT(leaderboard_id, user_id) DO UPDATE SET
				score      = MAX(excluded.score, score),
				updated_at = excluded.updated_at`,
			r.lbID, newUID, r.score, nowTs(),
		)
	}

	// Copy device records to new account.
	// Must be done before DeleteUserData wipes the old account's devices rows.
	// Collect into memory first to avoid holding a rows cursor while calling
	// Exec (deadlocks on the single SQLite connection).
	type devRow struct {
		deviceUID   string
		deviceOS    string
		deviceModel string
		platform    string
		updatedAt   int64
	}
	var devRows []devRow
	if rows, err := d.db.Query(
		`SELECT device_uid, device_os, device_model, platform, updated_at FROM devices WHERE user_id=?`, oldUID,
	); err == nil {
		for rows.Next() {
			var r devRow
			if rows.Scan(&r.deviceUID, &r.deviceOS, &r.deviceModel, &r.platform, &r.updatedAt) == nil {
				devRows = append(devRows, r)
			}
		}
		rows.Close()
	}
	for _, r := range devRows {
		d.db.Exec(`
			INSERT OR REPLACE INTO devices(user_id, device_uid, device_os, device_model, platform, updated_at)
			VALUES(?,?,?,?,?,?)`,
			newUID, r.deviceUID, r.deviceOS, r.deviceModel, r.platform, r.updatedAt,
		)
	}

	// Update Discord link to point at new account
	d.db.Exec(
		`UPDATE discord_links SET game_user_id=?, linked_at=? WHERE game_user_id=?`,
		newUID, nowTs(), oldUID,
	)

	// Wipe old account
	return d.DeleteUserData(oldUID)
}
