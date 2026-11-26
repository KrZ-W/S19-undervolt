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
			// measured_hashrate: our own 90 % edit (lowerStoredHashrate) is not a Braiins change either
			if k != "secs_since_epoch" && k != "nanos_since_epoch" && k != "created_on" && k != "measured_hashrate" {
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

// ---- stored hashrate at 90 % ----
//
// Braiins re-checks a finished tune about every 30 min (stage 4): each board's mcr over ~8 min against the
// hashrate stored in the tune ("measured_hashrate", measured by Braiins at stock voltage). A board ~3-4 points
// below it counts a strike; after 5 strikes Braiins declares the profile failed and retunes from scratch — with
// our trim still on, so its bootstrap fails and it saves a degraded tune (2026-11-26, the 2000 W level). The
// trim costs a few % of hashrate by design, so every tune's stored hashrate is set to storedHashrateFactor of what
// Braiins measured (owner rule, proven 2026-11-22 at 1900 W: no retune under trim). Which tunes are done is kept
// as the stored total we wrote: Braiins re-stamps created_on on a mere reload (same numbers), so the timestamp
// cannot tell; a different stored total means Braiins measured afresh (retune) -> lowered again. Only with
// bosminer STOPPED.

const storedHashrateFactor = 0.90

var loweredTunesFile = "/etc/s19volt/tunes-lowered.json" // key -> the stored total GH/s we wrote

// storedTotal: the tune's stored total hashrate (GH/s), 0 = none.
func storedTotal(e interface{}) float64 {
	g, _ := dig(e, "stage", "S4", "multi_configuration", "measured_hashrate", "GigaHashes").(float64)
	return g
}

// lowered: the tune still holds the total we wrote (Braiins' JSON round-trip may move the last digits).
func lowered(done map[string]float64, k string, e interface{}) bool {
	w, ok := done[k]
	return ok && math.Abs(storedTotal(e)-w) < 0.5
}

// lowerStoredHashrate sets every saved tune not lowered yet (or measured afresh by Braiins since) to 90 % of its stored
// hashrate, per board and in total. Returns the lowered keys. bosminer must be stopped.
func lowerStoredHashrate() ([]string, error) {
	b, err := os.ReadFile(autotuneFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	done := map[string]float64{}
	if lb, err := os.ReadFile(loweredTunesFile); err == nil {
		_ = json.Unmarshal(lb, &done)
	}
	boards, _ := doc["boards"].(map[string]interface{})
	var changed []string
	for k, v := range boards {
		e, _ := v.(map[string]interface{})
		if lowered(done, k, e) {
			continue
		}
		mc, _ := dig(e, "stage", "S4", "multi_configuration").(map[string]interface{})
		if mc == nil {
			continue // not a finished tune (no stage-4 entry): left as it is
		}
		scale := func(m interface{}) {
			if h, ok := m.(map[string]interface{}); ok {
				if g, ok := h["GigaHashes"].(float64); ok {
					h["GigaHashes"] = g * storedHashrateFactor
				}
			}
		}
		cfgs, _ := mc["configurations"].([]interface{})
		for _, c := range cfgs {
			if cm, ok := c.(map[string]interface{}); ok {
				scale(cm["measured_hashrate"])
			}
		}
		scale(mc["measured_hashrate"])
		done[k] = storedTotal(e)
		changed = append(changed, k)
	}
	for k := range done { // tunes Braiins dropped: forget them
		if _, ok := boards[k]; !ok {
			delete(done, k)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	backupTunes(b)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	tmp := autotuneFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, autotuneFile); err != nil {
		return nil, err
	}
	lb, _ := json.Marshal(done)
	_ = os.WriteFile(loweredTunesFile, lb, 0o600)
	sort.Strings(changed)
	return changed, nil
}

// tunesPending: a saved tune not lowered to 90 % yet (read-only; bosminer may be running).
func tunesPending() bool {
	b, err := os.ReadFile(autotuneFile)
	if err != nil {
		return false
	}
	var doc map[string]interface{}
	if json.Unmarshal(b, &doc) != nil {
		return false
	}
	done := map[string]float64{}
	if lb, err := os.ReadFile(loweredTunesFile); err == nil {
		_ = json.Unmarshal(lb, &done)
	}
	boards, _ := doc["boards"].(map[string]interface{})
	for k, v := range boards {
		if dig(v, "stage", "S4", "multi_configuration") != nil && !lowered(done, k, v) {
			return true
		}
	}
	return false
}
