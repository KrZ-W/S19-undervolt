package main

// profile.go — Braiins' saved tunes (/etc/bosminer-autotune.json). Only ever edited with bosminer STOPPED
// (it rewrites the file from memory while running), always after a backup. Used to scale a level's clocks
// without asking Braiins for a new tune (the entry loads straight into stage 4 on start).
//
// Format (version 9): boards["<W>W-<fp>:76-...-11"].stage.S4.multi_configuration.configurations[board]
//   .params.chip_params_set.inner[chip].pll_frequencies[0] = Hz, .params.voltage = mV

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// scaleProfileClocks multiplies every chip clock of the saved S4 entry for power target t (with nBoards boards)
// by f. Returns the number of chips changed. bosminer must be stopped.
func scaleProfileClocks(t, nBoards int, f float64) (int, error) {
	if f < 0.8 || f > 1.2 {
		return 0, fmt.Errorf("clock factor %.3f outside 0.8..1.2", f)
	}
	b, err := os.ReadFile(autotuneFile)
	if err != nil {
		return 0, err
	}
	backupTunes(b)
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range profileEntries(doc, t, nBoards) {
		s4, _ := dig(e, "stage", "S4", "multi_configuration").(map[string]interface{})
		cfgs, _ := s4["configurations"].([]interface{})
		for _, c := range cfgs {
			inner, _ := dig(c, "params", "chip_params_set", "inner").([]interface{})
			for _, ch := range inner {
				m, _ := ch.(map[string]interface{})
				fr, _ := m["pll_frequencies"].([]interface{})
				for i, x := range fr {
					if hz, ok := x.(float64); ok && hz > 0 {
						fr[i] = math.Round(hz * f)
						n++
					}
				}
			}
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no saved stage-4 entry for %d W with %d boards", t, nBoards)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}
	return n, os.WriteFile(autotuneFile, out, 0o644)
}

// profileEntriesSaved: a deep copy of the saved entries for target t / nBoards (by key), to put back later with
// restoreProfileEntries. Error if there is none.
func profileEntriesSaved(t, nBoards int) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(autotuneFile)
	if err != nil {
		return nil, err
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for k, e := range profileEntries(doc, t, nBoards) {
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out[k] = raw
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no saved stage-4 entry for %d W with %d boards", t, nBoards)
	}
	return out, nil
}

// restoreProfileEntries writes saved entries back into the autotune file under their keys (other entries kept),
// after a backup. bosminer must be stopped.
func restoreProfileEntries(saved map[string]json.RawMessage) error {
	if len(saved) == 0 {
		return fmt.Errorf("nothing to restore")
	}
	b, err := os.ReadFile(autotuneFile)
	if err != nil {
		return err
	}
	backupTunes(b)
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	boards, _ := doc["boards"].(map[string]interface{})
	if boards == nil {
		boards = map[string]interface{}{}
		doc["boards"] = boards
	}
	for k, raw := range saved {
		var e interface{}
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		boards[k] = e
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(autotuneFile, out, 0o644)
}

// backupTunes keeps a copy before every clock write, but only the 2 newest ("clock-*"): the miner's writable
// storage is ~55 MB in total and one tune file is ~400 KB. The wizard's first backup (moved tunes) is never pruned.
var tuneBackupDir = "/etc/s19volt/backup" // var: tests point it at a temp dir

func backupTunes(b []byte) {
	dir := tuneBackupDir
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "clock-"+time.Now().Format("2006-01-02-150405")+".json"), b, 0o600)
	old, _ := filepath.Glob(filepath.Join(dir, "clock-*.json"))
	sort.Strings(old)
	for len(old) > 2 {
		_ = os.Remove(old[0])
		old = old[1:]
	}
}

// profileEntries: the saved entries for power target t with nBoards boards, by key. Shared by the clock scaling
// and the profile guard.
func profileEntries(doc map[string]interface{}, t, nBoards int) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	boards, _ := doc["boards"].(map[string]interface{})
	for k, v := range boards {
		e, _ := v.(map[string]interface{})
		pt, _ := e["profile_target"].(map[string]interface{})
		chains, _ := e["chains"].([]interface{})
		if pw, _ := pt["Power"].(float64); e != nil && int(math.Round(pw)) == t && len(chains) == nBoards {
			out[k] = e
		}
	}
	return out
}

// stripStamps: profile content without bosminer's save timestamps (a re-save of the same tune is not a change).
func stripStamps(x interface{}) interface{} {
	switch v := x.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, y := range v {
			if k != "secs_since_epoch" && k != "nanos_since_epoch" && k != "created_on" {
				out[k] = stripStamps(y)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, y := range v {
			out[i] = stripStamps(y)
		}
		return out
	}
	return x
}

// profileSnapshot: the saved tune for target t / nBoards as canonical JSON without timestamps ("" = no entry).
// Read-only: the profile guard compares two snapshots to see whether bosminer changed its saved tune.
func profileSnapshot(t, nBoards int) (string, error) {
	b, err := os.ReadFile(autotuneFile)
	if err != nil {
		return "", err
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", err
	}
	es := profileEntries(doc, t, nBoards)
	if len(es) == 0 {
		return "", nil
	}
	m := map[string]interface{}{}
	for k, e := range es {
		m[k] = stripStamps(e)
	}
	out, err := json.Marshal(m) // map keys are sorted: canonical
	return string(out), err
}

func dig(x interface{}, keys ...string) interface{} {
	for _, k := range keys {
		m, ok := x.(map[string]interface{})
		if !ok {
			return nil
		}
		x = m[k]
	}
	return x
}
