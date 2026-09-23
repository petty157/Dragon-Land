package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// Daily login rewards — client reads user_data.daily_bonus on login and process_daily_reward.

const dailyBonusDayCount = 5

var dailyBonusGemRewards = []int{1, 2, 3, 4, 5}

func utcDayNumber(t time.Time) int64 {
	return t.UTC().Unix() / 86400
}

func dailyBonusUUID(userID int64, day int) string {
	return fmt.Sprintf("%d-day-%d", userID, day)
}

func buildDailyBonusLoginList(userID int64) []interface{} {
	st, err := db.GetDailyBonusState(userID)
	if err != nil {
		log.Printf("daily_bonus: load user=%d: %v", userID, err)
		return nil
	}

	today := utcDayNumber(time.Now())
	if st.LastClaimDay == today {
		return nil
	}

	cycleDay := st.CycleDay
	if cycleDay < 1 || cycleDay > dailyBonusDayCount {
		cycleDay = 1
	}
	if st.LastClaimDay > 0 && today > st.LastClaimDay+1 {
		cycleDay = 1
	}

	out := make([]interface{}, 0, dailyBonusDayCount)
	for day := 1; day <= dailyBonusDayCount; day++ {
		processed := day < cycleDay
		gems := dailyBonusGemRewards[day-1]
		out = append(out, map[string]interface{}{
			"uuid":      dailyBonusUUID(userID, day),
			"processed": processed,
			"reward": map[string]interface{}{
				"c": gems,
			},
		})
	}
	return out
}

func isFirstSessionUser(data map[string]interface{}) bool {
	if data == nil {
		return false
	}
	if v, ok := data["_firstSession"]; ok {
		return toBool(v)
	}
	return isFreshAccountProgress(data)
}

func attachLoginUserMeta(userData map[string]interface{}, userID int64) {
	if userData == nil {
		return
	}
	if _, ok := userData["session_count"]; !ok {
		userData["session_count"] = int64(1)
	}
	if isFirstSessionUser(userData) {
		userData["daily_bonus"] = []interface{}{}
		return
	}
	list := buildDailyBonusLoginList(userID)
	if len(list) > 0 {
		userData["daily_bonus"] = list
	} else {
		userData["daily_bonus"] = []interface{}{}
	}
}

func cmdProcessDailyReward(args map[string]interface{}, userID int64) map[string]interface{} {
	uuid := getStr(args, "reward_uuid")
	if uuid == "" {
		return map[string]interface{}{"ok": false, "error": "missing reward_uuid"}
	}

	if stateRaw, err := db.GetUserState(userID); err == nil && stateRaw != "" {
		var state map[string]interface{}
		if json.Unmarshal([]byte(stateRaw), &state) == nil && isFirstSessionUser(state) {
			return map[string]interface{}{"ok": true, "skipped": true}
		}
	}

	st, err := db.GetDailyBonusState(userID)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": "db_error"}
	}

	today := utcDayNumber(time.Now())
	if st.LastClaimDay == today {
		return map[string]interface{}{"ok": true, "already_claimed": true}
	}

	cycleDay := st.CycleDay
	if cycleDay < 1 || cycleDay > dailyBonusDayCount {
		cycleDay = 1
	}
	if st.LastClaimDay > 0 && today > st.LastClaimDay+1 {
		cycleDay = 1
	}

	want := dailyBonusUUID(userID, cycleDay)
	if uuid != want {
		log.Printf("daily_bonus: user=%d bad uuid got=%q want=%q cycleDay=%d", userID, uuid, want, cycleDay)
		return map[string]interface{}{"ok": false, "error": "invalid_reward"}
	}

	st.LastClaimDay = today
	if cycleDay >= dailyBonusDayCount {
		st.CycleDay = 1
	} else {
		st.CycleDay = cycleDay + 1
	}
	if err := db.SaveDailyBonusState(userID, st); err != nil {
		log.Printf("daily_bonus: save user=%d: %v", userID, err)
		return map[string]interface{}{"ok": false, "error": "db_error"}
	}

	log.Printf("daily_bonus: user=%d claimed day=%d gems=%d next_cycle_day=%d", userID, cycleDay, dailyBonusGemRewards[cycleDay-1], st.CycleDay)
	return map[string]interface{}{"ok": true}
}
