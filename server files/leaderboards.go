package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const mpTournamentGamedataID = "tournament_1"

func toBool(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	case json.Number:
		n, _ := x.Int64()
		return n != 0
	default:
		return toInt64(v) != 0
	}
}

func isFreshAccountProgress(data map[string]interface{}) bool {
	if v, ok := data["_maxLevelCompleted"]; ok {
		if toInt64(v) > 0 {
			return false
		}
	}
	if eps, ok := data["Episodes"].(map[string]interface{}); ok && len(eps) > 0 {
		return false
	}
	return true
}

// multiplayerEnabled — flip to true when MP ships in a future update.
func multiplayerEnabled() bool {
	return false
}

func multiplayerDisabledResponse() map[string]interface{} {
	return map[string]interface{}{
		"ok":          false,
		"error":       "multiplayer_disabled",
		"coming_soon": true,
	}
}

func collectFriendIDs(userID int64, filters map[string]interface{}) map[int64]bool {
	friendSet := map[int64]bool{userID: true}
	dbFriends, _ := db.GetFriends(userID)
	for _, id := range dbFriends {
		friendSet[id] = true
	}
	for _, key := range []string{"fb_friends", "gc_friends", "gp_friends"} {
		for _, s := range strings.Split(getStr(filters, key), ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
				friendSet[id] = true
			}
		}
	}
	return friendSet
}

func friendIDsFromSet(friendSet map[int64]bool) []int64 {
	ids := make([]int64, 0, len(friendSet))
	for id := range friendSet {
		ids = append(ids, id)
	}
	return ids
}

func loadUserStateMap(userID int64) map[string]interface{} {
	stateRaw, err := db.GetUserState(userID)
	if err != nil || stateRaw == "" {
		return nil
	}
	state := map[string]interface{}{}
	dec := json.NewDecoder(bytes.NewReader([]byte(stateRaw)))
	dec.UseNumber()
	if dec.Decode(&state) != nil {
		return nil
	}
	return state
}

func campaignNodeForUser(userID int64) int64 {
	state := loadUserStateMap(userID)
	if state == nil {
		return 0
	}
	if n := getInt64(state, "_maxLevelReached"); n > 0 {
		return n
	}
	return getInt64(state, "_maxLevelCompleted")
}

func dragonLevelFromDragonEntry(dragonID int64, dm map[string]interface{}) int64 {
	if dm == nil {
		return 1
	}
	lvl := toInt64(dm["Level"])
	if lvl < 1 {
		lvl = toInt64(dm["level"])
	}
	if lvl < 1 {
		lvl = 1
	}
	return lvl
}

func rankingDragonProfile(userID int64) (dragonID int64, dragonLevel int64) {
	state := loadUserStateMap(userID)
	if state == nil {
		return 0, 1
	}
	dragonID = getInt64(state, "_currentDragonId")
	dragons, _ := state["_dragons"].(map[string]interface{})
	if dragons != nil && dragonID >= 0 {
		if dm, ok := dragons[fmt.Sprintf("%d", dragonID)].(map[string]interface{}); ok {
			return dragonID, dragonLevelFromDragonEntry(dragonID, dm)
		}
	}
	var bestID, bestLvl int64 = 0, 1
	if dragons != nil {
		for key, raw := range dragons {
			dm, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			id := getInt64(dm, "Id")
			if id == 0 {
				if parsed, err := strconv.ParseInt(key, 10, 64); err == nil {
					id = parsed
				}
			}
			lvl := dragonLevelFromDragonEntry(id, dm)
			if lvl > bestLvl {
				bestLvl = lvl
				bestID = id
			}
		}
	}
	return bestID, bestLvl
}

func lastHighScoreDaysAgo(updatedAtUnix int64) int64 {
	if updatedAtUnix <= 0 {
		return 0
	}
	days := (time.Now().Unix() - updatedAtUnix) / 86400
	if days < 0 {
		return 0
	}
	return days
}

func buildRankingRow(e LeaderboardEntry, position int, category string, leaderboardID string) map[string]interface{} {
	name, _ := db.GetUserName(e.UserID)
	node := campaignNodeForUser(e.UserID)
	if leaderboardID == "CAMPAIGN" || leaderboardID == "CAMPAIGN_FRIENDS" {
		node = campaignNodeForUser(e.UserID)
	}
	dragonID, dragonLevel := rankingDragonProfile(e.UserID)
	return map[string]interface{}{
		"id":                 e.UserID,
		"name":               displayName(e.UserID, name),
		"score":              e.Score,
		"position":           position,
		"level":              dragonLevel,
		"dragon":             dragonID,
		"skin":               0,
		"category":           category,
		"node":               node,
		"MPScore":            e.Score,
		"promotion":          0,
		"last_high_score_at": lastHighScoreDaysAgo(0),
		"external_provider":  "",
		"external_id":        0,
		"fake":               false,
	}
}

func buildLeaderboardBlock(leaderboardID string, userID int64, friendSet map[int64]bool, friendsOnly bool, categoryLabel string) map[string]interface{} {
	var entries []LeaderboardEntry
	if friendsOnly {
		entries, _ = db.GetSocialLeaderboard(leaderboardID, friendIDsFromSet(friendSet))
	} else {
		entries, _, _ = db.GetCampaignLeaderboard(leaderboardID, userID, 200)
	}

	ranking := make([]interface{}, 0, len(entries))
	var me map[string]interface{}
	for i, e := range entries {
		row := buildRankingRow(e, i+1, categoryLabel, leaderboardID)
		ranking = append(ranking, row)
		if e.UserID == userID {
			me = row
		}
	}
	if me == nil {
		score, _ := db.GetCampaignScore(leaderboardID, userID)
		name, _ := db.GetUserName(userID)
		dragonID, dragonLevel := rankingDragonProfile(userID)
		me = map[string]interface{}{
			"id":                 userID,
			"name":               displayName(userID, name),
			"score":              score,
			"position":           len(ranking) + 1,
			"level":              dragonLevel,
			"dragon":             dragonID,
			"skin":               0,
			"category":           categoryLabel,
			"node":               campaignNodeForUser(userID),
			"MPScore":            score,
			"promotion":          0,
			"last_high_score_at": 0,
			"external_provider":  "",
			"external_id":        0,
			"fake":               false,
		}
	}
	return map[string]interface{}{"ranking": ranking, "user": me}
}

func emptyMultiplayerLeaderboardBlock(userID int64) map[string]interface{} {
	name, _ := db.GetUserName(userID)
	dragonID, dragonLevel := rankingDragonProfile(userID)
	me := map[string]interface{}{
		"id":                 userID,
		"name":               displayName(userID, name),
		"score":              int64(0),
		"position":           1,
		"level":              dragonLevel,
		"dragon":             dragonID,
		"skin":               0,
		"category":           "global",
		"node":               0,
		"MPScore":            int64(0),
		"promotion":          0,
		"last_high_score_at": 0,
		"external_provider":  "",
		"external_id":        0,
		"fake":               false,
	}
	return map[string]interface{}{
		"ranking": []interface{}{},
		"user":    me,
	}
}

// buildSocialLeaderboardsResponse — in-game ranking UI (campaign + fast track). MP tab empty until next update.
func buildSocialLeaderboardsResponse(userID int64, filters map[string]interface{}) map[string]interface{} {
	friendSet := collectFriendIDs(userID, filters)
	emptyMP := emptyMultiplayerLeaderboardBlock(userID)

	return map[string]interface{}{
		"tournament_info": map[string]interface{}{
			"tournament_id":     mpTournamentGamedataID,
			"remaining_seconds": int64(0),
			"win_streak":        int64(0),
		},
		"multiplayer": map[string]interface{}{
			"global":  emptyMP,
			"friends": emptyMP,
		},
		"campaign": map[string]interface{}{
			"global":  buildLeaderboardBlock("CAMPAIGN", userID, friendSet, false, "bronze"),
			"friends": buildLeaderboardBlock("CAMPAIGN", userID, friendSet, true, "bronze"),
		},
		"fast_track": map[string]interface{}{
			"global":  buildLeaderboardBlock("FAST_TRACK", userID, friendSet, false, "bronze"),
			"friends": buildLeaderboardBlock("FAST_TRACK", userID, friendSet, true, "bronze"),
		},
	}
}
