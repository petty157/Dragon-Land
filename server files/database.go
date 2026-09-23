package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// resolveDatabasePath picks the SQLite file (env DRAGONLAND_DB_PATH, else game_server.db in cwd, else data/).
func resolveDatabasePath() string {
	if p := strings.TrimSpace(os.Getenv("DRAGONLAND_DB_PATH")); p != "" {
		return filepath.Clean(p)
	}
	if _, err := os.Stat("game_server.db"); err == nil {
		return "game_server.db"
	}
	preferred := filepath.Join("data", "game_server.db")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	_ = os.MkdirAll("data", 0o755)
	return "game_server.db"
}

type Database struct {
	db *sql.DB
}

func NewDatabase(path string) (*Database, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	d := &Database{db: db}
	db.SetMaxOpenConns(1) // SQLite only supports one writer at a time
	if err := d.initSchema(); err != nil {
		return nil, err
	}
	if err := d.initMultiplayerSchema(); err != nil {
		return nil, err
	}
	if err := d.initModerationSchema(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Database) initSchema() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY,
			username      TEXT    NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			last_login_at INTEGER NOT NULL DEFAULT 0
		);

		-- Migration: add username column if it doesn't exist yet
		-- (safe to run multiple times; ALTER TABLE fails silently via OR IGNORE)

		CREATE TABLE IF NOT EXISTS user_progress (
			user_id    INTEGER PRIMARY KEY,
			state      TEXT    NOT NULL DEFAULT '{}',
			updated_at INTEGER NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		);

		CREATE TABLE IF NOT EXISTS sessions (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id        INTEGER NOT NULL,
			session_id     INTEGER NOT NULL UNIQUE,
			security_token TEXT,
			platform       TEXT,
			created_at     INTEGER NOT NULL,
			expires_at     INTEGER NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		);

		CREATE TABLE IF NOT EXISTS devices (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id      INTEGER NOT NULL,
			device_uid   TEXT,
			device_os    TEXT,
			device_model TEXT,
			platform     TEXT,
			updated_at   INTEGER NOT NULL,
			UNIQUE(user_id, device_uid)
		);

		CREATE TABLE IF NOT EXISTS push_tokens (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id    INTEGER NOT NULL,
			token      TEXT    NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(user_id, token)
		);

		-- Campaign & fast-track leaderboard scores (one row per user per leaderboard_id).
		-- leaderboard_id values used by the game: "CAMPAIGN", "FAST_TRACK",
		-- "CAMPAIGN_FRIENDS", "FAST_TRACK_FRIENDS".
		CREATE TABLE IF NOT EXISTS campaign_leaderboard (
			leaderboard_id TEXT    NOT NULL,
			user_id        INTEGER NOT NULL,
			score          INTEGER NOT NULL DEFAULT 0,
			updated_at     INTEGER NOT NULL,
			PRIMARY KEY (leaderboard_id, user_id)
		);
		CREATE INDEX IF NOT EXISTS idx_campaign_lb_score ON campaign_leaderboard(leaderboard_id, score DESC);

		CREATE TABLE IF NOT EXISTS ccu_snapshots (
			ts      INTEGER PRIMARY KEY,
			online  INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS discord_link_bonus (
			user_id    INTEGER PRIMARY KEY,
			granted_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		return err
	}

	// Add username column to existing databases that were created before this column existed.
	_, _ = d.db.Exec(`ALTER TABLE users ADD COLUMN username TEXT NOT NULL DEFAULT ''`)

	// ── Schema migrations table — each migration runs exactly once ────────────
	_, _ = d.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			id         TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`)

	d.runMigrationOnce("inbox_type_remap_v2", func() {
		// Remap ALL legacy/shorthand type strings to the exact enum values the
		// DLL's Enum.Parse expects in InboxMessageData.ParseListValues.
		// Any unrecognised string causes a TypeLoadException on the client.
		for _, pair := range [][2]string{
			// old shorthand                 exact DLL InboxMessageType enum value
			{"system",                       "InboxMessageSystemNews"},
			{"discord_verify",               "InboxMessageSystemNews"},
			{"news",                         "InboxMessageSystemNews"},
			{"life_sent",                    "InboxMessageLifeReceived"},
			{"send_life",                    "InboxMessageLifeReceived"},
			{"life_request",                 "InboxMessageLifeHelpRequest"},
			{"ask_for_life",                 "InboxMessageLifeHelpRequest"},
			{"friend_request",               "InboxMessageOnlineFriendRequest"},
			{"online_friend_request",        "InboxMessageOnlineFriendRequest"},
			{"multiplayer",                  "InboxMessageJoinMultiplayerRequest"},
			{"tournament_finished",          "InboxMessageTournamentEnd"},
			{"tournament_finished_reward",   "InboxMessageTournamentEndReward"},
			{"tournament_promotion",         "InboxMessageTournamentPromotion"},
			{"tournament_demotion",          "InboxMessageTournamentDemotion"},
			{"upgrade_reward",               "InboxMessageSystemNewsReward"},
			{"add_resource",                 "InboxMessageSystemNewsReward"},
			{"ENERGY_RECEIVED",              "InboxMessageLifeReceived"},
			{"tournament_goal_completed",    "InboxMessageTournamentReward"},
		} {
			if res, e := d.db.Exec(`UPDATE inbox_messages SET type=? WHERE type=?`, pair[1], pair[0]); e == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					log.Printf("inbox migration: remapped %d rows %q -> %q", n, pair[0], pair[1])
				}
			}
		}
		// Purge truly unrecognisable types (empty string or totally unknown).
		// NOTE: only delete rows whose type is empty — valid known types are kept.
		// We no longer delete rows with unknown-but-non-empty types to avoid
		// accidentally wiping messages after a server downgrade.
		if res, e := d.db.Exec(`DELETE FROM inbox_messages WHERE type=''`); e == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				log.Printf("inbox migration: purged %d rows with empty type", n)
			}
		}
	})

	d.runMigrationOnce("inbox_blob_cleanup_v1", func() {
		// Scrub stale "type" keys from data JSON blobs so buildInboxItem can't
		// have the blob overwrite the correctly-migrated type column.
		blobRows, e := d.db.Query(`SELECT id, data FROM inbox_messages WHERE data LIKE '%"type"%'`)
		if e != nil {
			return
		}
		type blobRow struct {
			id   int64
			data string
		}
		var toFix []blobRow
		for blobRows.Next() {
			var r blobRow
			blobRows.Scan(&r.id, &r.data)
			toFix = append(toFix, r)
		}
		blobRows.Close()
		for _, r := range toFix {
			var blob map[string]interface{}
			if json.Unmarshal([]byte(r.data), &blob) == nil {
				if _, has := blob["type"]; has {
					delete(blob, "type")
					cleaned, _ := json.Marshal(blob)
					if _, e2 := d.db.Exec(`UPDATE inbox_messages SET data=? WHERE id=?`, string(cleaned), r.id); e2 == nil {
						log.Printf("inbox migration: stripped stale 'type' key from blob id=%d", r.id)
					}
				}
			}
		}

		// Scrub empty subject_ext / body_ext — Enum.Parse throws on "".
		extRows, e2 := d.db.Query(`SELECT id, data FROM inbox_messages WHERE data LIKE '%_ext%'`)
		if e2 != nil {
			return
		}
		var toFix2 []blobRow
		for extRows.Next() {
			var r blobRow
			extRows.Scan(&r.id, &r.data)
			toFix2 = append(toFix2, r)
		}
		extRows.Close()
		for _, r := range toFix2 {
			var blob map[string]interface{}
			if json.Unmarshal([]byte(r.data), &blob) == nil {
				changed := false
				for _, key := range []string{"subject_ext", "body_ext"} {
					if v, ok := blob[key].(string); ok && v == "" {
						delete(blob, key)
						changed = true
					}
				}
				if changed {
					cleaned, _ := json.Marshal(blob)
					if _, e3 := d.db.Exec(`UPDATE inbox_messages SET data=? WHERE id=?`, string(cleaned), r.id); e3 == nil {
						log.Printf("inbox migration: removed empty ext fields from blob id=%d", r.id)
					}
				}
			}
		}
	})

	d.runMigrationOnce("daily_bonus_state_v1", func() {
		_, err := d.db.Exec(`
			CREATE TABLE IF NOT EXISTS daily_bonus_state (
				user_id         INTEGER PRIMARY KEY,
				cycle_day       INTEGER NOT NULL DEFAULT 1,
				last_claim_day  INTEGER NOT NULL DEFAULT 0,
				updated_at      INTEGER NOT NULL DEFAULT 0
			)`)
		if err != nil {
			log.Printf("daily_bonus_state migration: %v", err)
		}
	})

	return nil
}

// runMigrationOnce executes fn only if migration `id` has not been applied yet.
func (d *Database) runMigrationOnce(id string, fn func()) {
	var count int
	d.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id=?`, id).Scan(&count)
	if count > 0 {
		return
	}
	fn()
	d.db.Exec(`INSERT OR IGNORE INTO schema_migrations(id, applied_at) VALUES(?,?)`, id, nowTs())
	log.Printf("migration applied: %s", id)
}

func nowTs() int64 { return time.Now().Unix() }

// ── Users ──────────────────────────────────────────────────────────────────

func (d *Database) EnsureUser(userID int64) (isNew bool, err error) {
	res, err := d.db.Exec(
		`INSERT OR IGNORE INTO users(id, username, created_at, last_login_at) VALUES(?,?,?,?)`,
		userID, "", nowTs(), nowTs(),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (d *Database) TouchLogin(userID int64) error {
	_, err := d.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, nowTs(), userID)
	return err
}

// GetTotalUsers returns the total number of registered users.
func (d *Database) GetTotalUsers() (int64, error) {
	var count int64
	err := d.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

// GetOnlinePlayers returns users active in the last 2 minutes based on last_login_at.
// The game sends sync/heartbeat packets constantly so this reflects real-time activity.
func (d *Database) GetOnlinePlayers() (int64, error) {
	var count int64
	cutoff := nowTs() - 2*60
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE last_login_at >= ?`, cutoff,
	).Scan(&count)
	return count, err
}

// GetNewUsersToday returns users registered since midnight UTC today.
func (d *Database) GetNewUsersToday() (int64, error) {
	var count int64
	now := nowTs()
	midnight := now - (now % 86400)
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE created_at >= ?`, midnight,
	).Scan(&count)
	return count, err
}

// SetUserName persists the display name for a user.
func (d *Database) SetUserName(userID int64, name string) error {
	_, err := d.db.Exec(`UPDATE users SET username = ? WHERE id = ?`, name, userID)
	return err
}

// GetUserName returns the display name for a user (empty string if none set).
func (d *Database) GetUserName(userID int64) (string, error) {
	var name string
	err := d.db.QueryRow(`SELECT username FROM users WHERE id = ?`, userID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// ── User progress ──────────────────────────────────────────────────────────

func (d *Database) SaveUserState(userID int64, raw string) error {
	_, err := d.db.Exec(`
		INSERT INTO user_progress(user_id, state, updated_at) VALUES(?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`,
		userID, raw, nowTs(),
	)
	return err
}

func (d *Database) GetUserState(userID int64) (string, error) {
	row := d.db.QueryRow(`SELECT state FROM user_progress WHERE user_id = ?`, userID)
	var state string
	err := row.Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return state, err
}

// BuildUserData builds the login-response user blob.
// Returning users get their last synced state. New users get a default scaffold.
func (d *Database) BuildUserData(userID int64) (map[string]interface{}, error) {
	state, err := d.GetUserState(userID)
	if err != nil {
		return nil, err
	}

	name, _ := d.GetUserName(userID)

	if state != "" {
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(state), &data); err != nil {
			return nil, fmt.Errorf("corrupt progress blob for user %d: %w", userID, err)
		}
		data["id"] = userID
		data["_firstSession"] = false
		// Always keep the server-stored username authoritative.
		if name != "" {
			data["_userName"] = name
		}
		return data, nil
	}

	// New user — full base state matching the HAR reference response exactly.
	base := map[string]interface{}{
		"CompletedGrindingLevels": map[string]interface{}{},
		"Episodes": map[string]interface{}{
			"episode_1": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 1, "MaxLevel": 1, "Status": 1, "StatusChanged": false, "Levels": map[string]interface{}{"1": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "2": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "3": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "4": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "5": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "6": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "7": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_2": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 21, "MaxLevel": 21, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"21": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "22": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "23": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "24": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "25": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "26": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "27": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "28": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "29": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "30": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "31": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "32": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "33": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "34": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_3": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 41, "MaxLevel": 41, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"41": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "42": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "43": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "44": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "45": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "46": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "47": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "48": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "49": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "50": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "51": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "52": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "53": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "54": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_4": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 61, "MaxLevel": 61, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"61": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "62": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "63": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "64": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "65": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "66": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "67": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "68": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "69": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "70": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "71": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "72": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "73": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "74": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_5": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 81, "MaxLevel": 81, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"81": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "82": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "83": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "84": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "85": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "86": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "87": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "88": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "89": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "90": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "91": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "92": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "93": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "94": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_6": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 101, "MaxLevel": 101, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"101": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "102": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "103": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "104": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "105": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "106": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "107": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "108": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "109": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "110": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "111": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "112": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "113": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "114": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_7": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 121, "MaxLevel": 121, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"121": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}, "122": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "123": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "124": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "125": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "126": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "127": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "128": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "129": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "130": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "131": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "132": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "133": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}, "134": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 0}}},
			"episode_8": map[string]interface{}{"BossUnlocked": false, "CompletedLevel": -1, "CompletedLevelAnimation": false, "CurrentLevel": 141, "MaxLevel": 141, "Status": 0, "StatusChanged": false, "Levels": map[string]interface{}{"141": map[string]interface{}{"_itemsPicked": map[string]interface{}{}, "_keysSpend": false, "_typeLevelComplete": 1}}},
		},
		"GameModes": map[string]interface{}{
			"EPISODE":      map[string]interface{}{"Status": 1, "StatusChanged": false},
			"MICRO_SERIES": map[string]interface{}{"Status": 0, "StatusChanged": false},
			"MULTIPLAYER":  map[string]interface{}{"Status": 0, "StatusChanged": false},
			"PLAY_SET":     map[string]interface{}{"Status": 0, "StatusChanged": false},
		},
		"GrindingCooldowns":           map[string]interface{}{},
		"MicroSeriesPlayed":            false,
		"MicroserieMaxCoins":           0,
		"MicroserieMaxCoinsPerSession": 0,
		"MicroseriesWayPoints":         map[string]interface{}{},
		"VideoAds": map[string]interface{}{
			"daily_count_live":     0,
			"first_video_ts_shop":  0,
			"indexes_shop_reward":  []interface{}{},
			"last_video_ts_live":   0,
			"reward_video_ad_shop": false,
		},
		"_attackLearned": false,
		"_blueCrystals":  0,
		"_cinematicsNodeMap": map[string]interface{}{
			"episode_1": true, "episode_2": true, "episode_3": true, "episode_4": true,
			"episode_5": true, "episode_6": true, "episode_7": true, "episode_8": true,
		},
		"_coins":                     0,
		"_currentDragonId":           0,
		"_currentLevelId":            0,
		"_dragons": map[string]interface{}{
			"0": map[string]interface{}{
				"Boost": 0, "DragonEvolutionTier": 0, "Experience": 0,
				"Id": 0, "OwnedSkins": []interface{}{0}, "SelectedSkin": 0,
			},
		},
		"_dragonsUnlocked": []interface{}{},
		"_dragonskins": map[string]interface{}{
			"0": map[string]interface{}{"OwnedSkins": []interface{}{0}},
		},
		"_energy":                   3,
		"_energyLostTime":           5400,
		"_firstSession":             true,
		"_gems":                     0,
		"_givenFacebookGift":        true,
		"_items":                    []interface{}{},
		"_keys":                     0,
		"_lastRateTime":             0,
		"_lastUpgradeTime":          0,
		"_lifeLostTime":             9000,
		"_lives":                    5,
		"_maxLevelCompleted":        0,
		"_maxLevelReached":          0,
		"_microseriesStartTimeStamp": 0,
		"_rated":                    false,
		"_redCrystals":              0,
		"_scoreCoins":               0,
		"_scoreMP":                  0,
		"_softGatchaGivenTS":        0,
		"_tutorialCompleted":        []interface{}{},
		"_tutorials":                []interface{}{0, 5, 12, 14, 28, 63, 70, 76, 85, 88, 95, 120, 130, 200, 202},
		"_userName":                 name,
		"_versionNumber":            2.5,
		"id":                        userID,
	}
	return base, nil
}

func (d *Database) CopyUserState(fromUID, toUID int64) error {
	state, err := d.GetUserState(fromUID)
	if err != nil || state == "" {
		return err
	}
	return d.SaveUserState(toUID, state)
}

// ── Sessions ───────────────────────────────────────────────────────────────

type Session struct {
	ID            int64  `json:"id"`
	UserID        int64  `json:"user_id"`
	SessionID     int64  `json:"session_id"`
	SecurityToken string `json:"security_token"`
	Platform      string `json:"platform"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"`
}

func (d *Database) CreateSession(userID, sessionID int64, securityToken, platform string, createdAt int64) error {
	// Expire any existing sessions for this user before creating a new one.
	// This ensures only one valid session exists at a time and stale sessions
	// from previous logins/installs are always kicked on the next login.
	_, _ = d.db.Exec(`UPDATE sessions SET expires_at = 0 WHERE user_id = ?`, userID)

	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO sessions(user_id, session_id, security_token, platform, created_at, expires_at)
		VALUES(?,?,?,?,?,?)`,
		userID, sessionID, securityToken, platform, createdAt, createdAt+86400,
	)
	return err
}

func (d *Database) GetSessionByID(sessionID int64) (*Session, error) {
	row := d.db.QueryRow(`
		SELECT id, user_id, session_id, security_token, platform, created_at, expires_at
		FROM sessions WHERE session_id = ?`, sessionID)
	s := &Session{}
	err := row.Scan(&s.ID, &s.UserID, &s.SessionID, &s.SecurityToken, &s.Platform, &s.CreatedAt, &s.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func (d *Database) ValidateSession(userID, sessionID int64) bool {
	row := d.db.QueryRow(
		`SELECT expires_at FROM sessions WHERE user_id = ? AND session_id = ?`, userID, sessionID)
	var exp int64
	if err := row.Scan(&exp); err != nil {
		return false
	}
	return nowTs() < exp
}

// InvalidateUserSessions immediately expires all sessions for a user by setting
// expires_at to 0. The next packet the game sends will be rejected with
// invalid_session, forcing the game to re-login and reload state from the server.
func (d *Database) InvalidateUserSessions(userID int64) error {
	_, err := d.db.Exec(`UPDATE sessions SET expires_at = 0 WHERE user_id = ?`, userID)
	return err
}

// GetUserIDsByDeviceUID returns all user_ids that have ever logged in from a
// given device_uid. Used to invalidate sessions for all accounts on a device
// when that device is blacklisted.
func (d *Database) GetUserIDsByDeviceUID(deviceUID string) ([]int64, error) {
	rows, err := d.db.Query(`SELECT DISTINCT user_id FROM devices WHERE device_uid = ?`, deviceUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ── Devices ────────────────────────────────────────────────────────────────

func (d *Database) UpsertDevice(userID int64, deviceUID, deviceOS, deviceModel, platform string) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO devices(user_id, device_uid, device_os, device_model, platform, updated_at)
		VALUES(?,?,?,?,?,?)`,
		userID, deviceUID, deviceOS, deviceModel, platform, nowTs(),
	)
	return err
}

// ── Push tokens ────────────────────────────────────────────────────────────

func (d *Database) UpsertPushToken(userID int64, token string) error {
	_, err := d.db.Exec(`
		INSERT INTO push_tokens(user_id, token, updated_at) VALUES(?,?,?)
		ON CONFLICT(user_id, token) DO UPDATE SET updated_at = excluded.updated_at`,
		userID, token, nowTs(),
	)
	return err
}

// ── Helpers ────────────────────────────────────────────────────────────────

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case json.Number:
		// UseNumber() decoder — precise int64, no float64 rounding
		n, _ := strconv.ParseInt(x.String(), 10, 64)
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		n, _ := parseInt64(x)
		return n
	}
	return 0
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func getStr(args map[string]interface{}, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func getInt64(args map[string]interface{}, key string) int64 {
	return toInt64(args[key])
}

func jsonMarshal(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ── Campaign leaderboard ───────────────────────────────────────────────────

// UpsertCampaignScore updates a leaderboard entry.
// For campaign/fast-track: only updates if the new score is higher (high-score semantics).
// For MP leaderboards (MP_GLOBAL, MP_FRIENDS, MP_TOURNAMENT): always overwrites
// because the caller (cmdFinishRace) already computed the cumulative total.
func (d *Database) UpsertCampaignScore(leaderboardID string, userID, score int64) (rank int64, err error) {
	isCumulative := leaderboardID == "MP_GLOBAL" || leaderboardID == "MP_FRIENDS" ||
		leaderboardID == "MP_TOURNAMENT" || leaderboardID == "MP_BRONZE" ||
		leaderboardID == "MP_SILVER" || leaderboardID == "MP_GOLD"

	scoreExpr := "MAX(excluded.score, score)"
	if isCumulative {
		scoreExpr = "excluded.score" // caller already added delta
	}

	_, err = d.db.Exec(`
		INSERT INTO campaign_leaderboard(leaderboard_id, user_id, score, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(leaderboard_id, user_id) DO UPDATE SET
			score      = `+scoreExpr+`,
			updated_at = excluded.updated_at`,
		leaderboardID, userID, score, nowTs(),
	)
	if err != nil {
		return 0, err
	}
	err = d.db.QueryRow(`
		SELECT COUNT(*) + 1
		FROM campaign_leaderboard
		WHERE leaderboard_id = ? AND score > (
			SELECT score FROM campaign_leaderboard WHERE leaderboard_id = ? AND user_id = ?
		)`, leaderboardID, leaderboardID, userID,
	).Scan(&rank)
	return rank, err
}

type LeaderboardEntry struct {
	UserID int64 `json:"user_id"`
	Score  int64 `json:"score"`
	Rank   int64 `json:"rank"`
}

// GetCampaignScore returns a user's current score on a leaderboard (0 if not present).
func (d *Database) GetCampaignScore(leaderboardID string, userID int64) (int64, error) {
	var score int64
	err := d.db.QueryRow(`SELECT score FROM campaign_leaderboard WHERE leaderboard_id=? AND user_id=?`,
		leaderboardID, userID).Scan(&score)
	if err != nil {
		return 0, nil // not found is fine — score is 0
	}
	return score, nil
}

// GetTournamentScore returns a user's current score in a tournament (0 if not present).
func (d *Database) GetTournamentScore(tournamentID, userID int64) (int64, error) {
	var score int64
	err := d.db.QueryRow(`SELECT score FROM tournament_scores WHERE tournament_id=? AND user_id=?`,
		tournamentID, userID).Scan(&score)
	if err != nil {
		return 0, nil
	}
	return score, nil
}

func (d *Database) GetCampaignLeaderboard(leaderboardID string, requestingUserID int64, limit int) ([]LeaderboardEntry, int64, error) {
	now := time.Now().Unix()
	rows, err := d.db.Query(`
		SELECT user_id, score,
			   ROW_NUMBER() OVER (ORDER BY score DESC) AS rank
		FROM campaign_leaderboard
		WHERE leaderboard_id = ?
		  AND user_id NOT IN (
			  SELECT user_id FROM bans WHERE ban_until = 0 OR ban_until > ?
		  )
		ORDER BY score DESC
		LIMIT ?`, leaderboardID, now, limit,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.Score, &e.Rank); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}

	var total int64
	d.db.QueryRow(`
		SELECT COUNT(*) FROM campaign_leaderboard
		WHERE leaderboard_id = ?
		  AND user_id NOT IN (
			  SELECT user_id FROM bans WHERE ban_until = 0 OR ban_until > ?
		  )`, leaderboardID, now).Scan(&total)

	return entries, total, nil
}

func (d *Database) SeedGameData(path string) {
	log.Printf("SeedGameData: config is loaded in-memory, skipping")
}

// ── Friends ────────────────────────────────────────────────────────────────

func (d *Database) initMultiplayerSchema() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS friends (
			user_id    INTEGER NOT NULL,
			friend_id  INTEGER NOT NULL,
			platform   TEXT    NOT NULL DEFAULT 'sp',
			created_at INTEGER NOT NULL,
			PRIMARY KEY (user_id, friend_id)
		);

		CREATE TABLE IF NOT EXISTS inbox_messages (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			to_user_id   INTEGER NOT NULL,
			from_user_id INTEGER NOT NULL,
			type         TEXT    NOT NULL,
			data         TEXT    NOT NULL DEFAULT '{}',
			created_at   INTEGER NOT NULL,
			read_at      INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_inbox_to ON inbox_messages(to_user_id, read_at);

		CREATE TABLE IF NOT EXISTS tournaments (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			category   TEXT    NOT NULL,
			state      TEXT    NOT NULL DEFAULT 'active',
			starts_at  INTEGER NOT NULL,
			ends_at    INTEGER NOT NULL,
			data       TEXT    NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS tournament_scores (
			tournament_id INTEGER NOT NULL,
			user_id       INTEGER NOT NULL,
			score         INTEGER NOT NULL DEFAULT 0,
			goals         TEXT    NOT NULL DEFAULT '{}',
			updated_at    INTEGER NOT NULL,
			PRIMARY KEY (tournament_id, user_id)
		);
	`)
	return err
}

func (d *Database) AddFriend(userID, friendID int64, platform string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowTs()
	for _, pair := range [][2]int64{{userID, friendID}, {friendID, userID}} {
		_, err = tx.Exec(`INSERT OR IGNORE INTO friends(user_id, friend_id, platform, created_at) VALUES(?,?,?,?)`,
			pair[0], pair[1], platform, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *Database) RemoveFriend(userID, friendID int64) error {
	_, err := d.db.Exec(`DELETE FROM friends WHERE (user_id=? AND friend_id=?) OR (user_id=? AND friend_id=?)`,
		userID, friendID, friendID, userID)
	return err
}

func (d *Database) GetFriends(userID int64) ([]int64, error) {
	rows, err := d.db.Query(`SELECT friend_id FROM friends WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

func (d *Database) IsFriend(userID, friendID int64) bool {
	var c int
	d.db.QueryRow(`SELECT COUNT(*) FROM friends WHERE user_id=? AND friend_id=?`, userID, friendID).Scan(&c)
	return c > 0
}

// ── Inbox ──────────────────────────────────────────────────────────────────

type InboxMsg struct {
	ID         int64                  `json:"id"`
	ToUserID   int64                  `json:"to_user_id"`
	FromUserID int64                  `json:"from_user_id"`
	Type       string                 `json:"type"`
	Data       map[string]interface{} `json:"data"`
	CreatedAt  int64                  `json:"created_at"`
	ReadAt     int64                  `json:"read_at"`
}

func (d *Database) SendInboxMessage(toUID, fromUID int64, msgType string, data map[string]interface{}) error {
	// Never persist subject_ext or body_ext as empty strings — the DLL's
	// Enum.Parse throws ArgumentException on "". Strip them at write time so
	// old rows can't sneak bad values back through buildInboxItem.
	for _, key := range []string{"subject_ext", "body_ext"} {
		if v, ok := data[key].(string); ok && v == "" {
			delete(data, key)
		}
	}
	raw, _ := json.Marshal(data)
	_, err := d.db.Exec(`INSERT INTO inbox_messages(to_user_id, from_user_id, type, data, created_at) VALUES(?,?,?,?,?)`,
		toUID, fromUID, msgType, string(raw), nowTs())
	return err
}

func (d *Database) GetInboxMessages(userID int64) ([]InboxMsg, error) {
	rows, err := d.db.Query(`
		SELECT id, to_user_id, from_user_id, type, data, created_at, read_at
		FROM inbox_messages WHERE to_user_id=? AND read_at=0 ORDER BY created_at DESC LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Only the exact InboxMessageType enum strings the DLL's Enum.Parse accepts.
	validInboxType := map[string]bool{
		"InboxMessageSystemNews":           true,
		"InboxMessageSystemNewsReward":     true,
		"InboxMessageLifeHelpRequest":      true,
		"InboxMessageLifeReceived":         true,
		"InboxMessageOnlineFriendRequest":  true,
		"InboxMessageJoinMultiplayerRequest": true,
		"InboxMessageTournamentEnd":        true,
		"InboxMessageTournamentEndReward":  true,
		"InboxMessageTournamentPromotion":  true,
		"InboxMessageTournamentDemotion":   true,
		"InboxMessageTournamentReward":     true,
	}
	var msgs []InboxMsg
	for rows.Next() {
		var m InboxMsg
		var rawData string
		rows.Scan(&m.ID, &m.ToUserID, &m.FromUserID, &m.Type, &rawData, &m.CreatedAt, &m.ReadAt)
		if !validInboxType[m.Type] {
			log.Printf("GetInboxMessages: skipping msg id=%d invalid type=%q", m.ID, m.Type)
			continue
		}
		json.Unmarshal([]byte(rawData), &m.Data)
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// GetInboxMessageByID fetches a single inbox message by its ID (any recipient).
func (d *Database) GetInboxMessageByID(msgID int64) (*InboxMsg, error) {
	var m InboxMsg
	var rawData string
	err := d.db.QueryRow(`
		SELECT id, to_user_id, from_user_id, type, data, created_at, read_at
		FROM inbox_messages WHERE id=?`, msgID,
	).Scan(&m.ID, &m.ToUserID, &m.FromUserID, &m.Type, &rawData, &m.CreatedAt, &m.ReadAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(rawData), &m.Data)
	return &m, nil
}

func (d *Database) MarkInboxRead(msgID int64) error {
	_, err := d.db.Exec(`UPDATE inbox_messages SET read_at=? WHERE id=?`, nowTs(), msgID)
	return err
}

func (d *Database) DeleteInboxMessage(msgID, userID int64) error {
	_, err := d.db.Exec(`DELETE FROM inbox_messages WHERE id=? AND to_user_id=?`, msgID, userID)
	return err
}

// ── Tournaments ────────────────────────────────────────────────────────────

type Tournament struct {
	ID       int64                  `json:"id"`
	Category string                 `json:"category"`
	State    string                 `json:"state"`
	StartsAt int64                  `json:"starts_at"`
	EndsAt   int64                  `json:"ends_at"`
	Data     map[string]interface{} `json:"data"`
}

func (d *Database) EnsureTournament(category string) (*Tournament, error) {
	now := nowTs()
	row := d.db.QueryRow(`SELECT id, category, state, starts_at, ends_at, data FROM tournaments
		WHERE category=? AND state='active' AND ends_at > ? ORDER BY id DESC LIMIT 1`, category, now)
	t := &Tournament{}
	var rawData string
	err := row.Scan(&t.ID, &t.Category, &t.State, &t.StartsAt, &t.EndsAt, &rawData)
	if err == nil {
		json.Unmarshal([]byte(rawData), &t.Data)
		return t, nil
	}
	startsAt := now
	endsAt := now + 7*24*3600
	res, err := d.db.Exec(`INSERT INTO tournaments(category, state, starts_at, ends_at, data, created_at) VALUES(?,?,?,?,?,?)`,
		category, "active", startsAt, endsAt, "{}", now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Tournament{ID: id, Category: category, State: "active", StartsAt: startsAt, EndsAt: endsAt, Data: map[string]interface{}{}}, nil
}

func (d *Database) GetActiveTournaments() ([]Tournament, error) {
	now := nowTs()
	rows, err := d.db.Query(`SELECT id, category, state, starts_at, ends_at, data FROM tournaments
		WHERE state='active' AND ends_at > ? ORDER BY category`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []Tournament
	for rows.Next() {
		var t Tournament
		var rawData string
		rows.Scan(&t.ID, &t.Category, &t.State, &t.StartsAt, &t.EndsAt, &rawData)
		json.Unmarshal([]byte(rawData), &t.Data)
		ts = append(ts, t)
	}
	return ts, nil
}

func (d *Database) UpsertTournamentScore(tournamentID, userID, score int64, goals map[string]interface{}) error {
	rawGoals, _ := json.Marshal(goals)
	_, err := d.db.Exec(`
		INSERT INTO tournament_scores(tournament_id, user_id, score, goals, updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(tournament_id, user_id) DO UPDATE SET
			score=MAX(excluded.score, score), goals=excluded.goals, updated_at=excluded.updated_at`,
		tournamentID, userID, score, string(rawGoals), nowTs())
	return err
}

type TournamentEntry struct {
	UserID int64 `json:"user_id"`
	Score  int64 `json:"score"`
	Rank   int64 `json:"rank"`
}

func (d *Database) GetTournamentLeaderboard(tournamentID int64, limit int) ([]TournamentEntry, error) {
	rows, err := d.db.Query(`
		SELECT user_id, score, ROW_NUMBER() OVER (ORDER BY score DESC) AS rank
		FROM tournament_scores WHERE tournament_id=? ORDER BY score DESC LIMIT ?`,
		tournamentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []TournamentEntry
	for rows.Next() {
		var e TournamentEntry
		rows.Scan(&e.UserID, &e.Score, &e.Rank)
		entries = append(entries, e)
	}
	return entries, nil
}

// GetSocialLeaderboard returns leaderboard filtered to a set of user IDs.
func (d *Database) GetSocialLeaderboard(leaderboardID string, friendIDs []int64) ([]LeaderboardEntry, error) {
	if len(friendIDs) == 0 {
		return []LeaderboardEntry{}, nil
	}
	placeholders := make([]string, len(friendIDs))
	args := []interface{}{leaderboardID}
	for i, id := range friendIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	inClause := strings.Join(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT user_id, score, ROW_NUMBER() OVER (ORDER BY score DESC) AS rank
		FROM campaign_leaderboard
		WHERE leaderboard_id=? AND user_id IN (%s)
		ORDER BY score DESC`, inClause)
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		rows.Scan(&e.UserID, &e.Score, &e.Rank)
		entries = append(entries, e)
	}
	return entries, nil
}

// DeleteUserData permanently wipes all data for a user (ban).
func (d *Database) DeleteUserData(userID int64) error {
	tables := []string{
		"user_progress",
		"sessions",
		"devices",
		"push_tokens",
		"campaign_leaderboard",
		"friends",
		"inbox_messages",
		"tournament_scores",
		"discord_link_bonus",
	}
	for _, table := range tables {
		_, err := d.db.Exec(`DELETE FROM `+table+` WHERE user_id = ?`, userID)
		if err != nil {
			log.Printf("DeleteUserData: table=%s user=%d err=%v", table, userID, err)
		}
	}
	// Delete the user row itself last
	_, err := d.db.Exec(`DELETE FROM users WHERE id = ?`, userID)
	return err
}

// SearchPlayerByUsername finds users whose username contains the query (case-insensitive).
// Returns up to 10 matches.
func (d *Database) SearchPlayerByUsername(query string) ([]struct {
	UserID   int64
	Username string
}, error) {
	rows, err := d.db.Query(
		`SELECT id, username FROM users WHERE LOWER(username) LIKE LOWER(?) AND username != '' ORDER BY username LIMIT 10`,
		"%"+query+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []struct {
		UserID   int64
		Username string
	}
	for rows.Next() {
		var r struct {
			UserID   int64
			Username string
		}
		rows.Scan(&r.UserID, &r.Username)
		results = append(results, r)
	}
	return results, nil
}

// UnlinkDiscord removes the Discord ↔ game account link for a given game user_id.
func (d *Database) UnlinkDiscord(gameUserID int64) error {
	_, err := d.db.Exec(`DELETE FROM discord_links WHERE game_user_id=?`, gameUserID)
	return err
}

// RawQuery executes a raw SELECT and returns rows as []map[string]interface{}.
// Used internally for flexible alt-account lookups.
func (d *Database) RawQuery(query string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make(map[string]interface{}, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		out = append(out, row)
	}
	return out, nil
}

// GetPlayerByID returns a single player's username and whether they exist.
func (d *Database) GetPlayerByID(userID int64) (string, bool, error) {
	var name string
	err := d.db.QueryRow(`SELECT username FROM users WHERE id = ?`, userID).Scan(&name)
	if err != nil {
		return "", false, nil // not found
	}
	return name, true, nil
}

// ── CCU snapshots ──────────────────────────────────────────────────────────

// RecordCCUSnapshot writes one CCU data point (called every minute by startCCUPoller).
func (d *Database) RecordCCUSnapshot(online int64) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO ccu_snapshots(ts, online) VALUES(?,?)`,
		nowTs(), online,
	)
	return err
}

// GetPeakCCU returns the all-time highest online count ever recorded.
func (d *Database) GetPeakCCU() (int64, error) {
	var peak int64
	err := d.db.QueryRow(`SELECT COALESCE(MAX(online),0) FROM ccu_snapshots`).Scan(&peak)
	return peak, err
}

// GetPeakCCU24h returns the highest online count in the last 24 hours.
func (d *Database) GetPeakCCU24h() (int64, error) {
	cutoff := nowTs() - 86400
	var peak int64
	err := d.db.QueryRow(
		`SELECT COALESCE(MAX(online),0) FROM ccu_snapshots WHERE ts >= ?`, cutoff,
	).Scan(&peak)
	return peak, err
}

// ── Daily bonus ────────────────────────────────────────────────────────────

type DailyBonusState struct {
	CycleDay     int
	LastClaimDay int64
}

func (d *Database) GetDailyBonusState(userID int64) (DailyBonusState, error) {
	var st DailyBonusState
	st.CycleDay = 1
	err := d.db.QueryRow(
		`SELECT cycle_day, last_claim_day FROM daily_bonus_state WHERE user_id = ?`, userID,
	).Scan(&st.CycleDay, &st.LastClaimDay)
	if err == sql.ErrNoRows {
		return st, nil
	}
	if st.CycleDay < 1 {
		st.CycleDay = 1
	}
	return st, err
}

func (d *Database) SaveDailyBonusState(userID int64, st DailyBonusState) error {
	if st.CycleDay < 1 {
		st.CycleDay = 1
	}
	if st.CycleDay > dailyBonusDayCount {
		st.CycleDay = 1
	}
	_, err := d.db.Exec(`
		INSERT INTO daily_bonus_state(user_id, cycle_day, last_claim_day, updated_at)
		VALUES(?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET
			cycle_day = excluded.cycle_day,
			last_claim_day = excluded.last_claim_day,
			updated_at = excluded.updated_at`,
		userID, st.CycleDay, st.LastClaimDay, nowTs(),
	)
	return err
}

// ── Discord link bonus ─────────────────────────────────────────────────────

// HasReceivedLinkBonus returns true if the user has already been given the
// one-time Discord link gem reward.
func (d *Database) HasReceivedLinkBonus(userID int64) bool {
	var c int
	d.db.QueryRow(`SELECT COUNT(*) FROM discord_link_bonus WHERE user_id=?`, userID).Scan(&c)
	return c > 0
}

// MarkLinkBonusGiven records that the user has received the link bonus so it
// is never granted a second time.
func (d *Database) MarkLinkBonusGiven(userID int64) error {
	_, err := d.db.Exec(
		`INSERT OR IGNORE INTO discord_link_bonus(user_id, granted_at) VALUES(?,?)`,
		userID, nowTs(),
	)
	return err
}
