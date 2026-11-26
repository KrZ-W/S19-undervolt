package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// tuneDoc: a minimal autotune file with one finished tune per (key, created, total GH/s, per-board GH/s).
func tuneDoc(t *testing.T, entries map[string][3]float64) string {
	boards := map[string]interface{}{}
	for k, e := range entries {
		boards[k] = map[string]interface{}{
			"profile_target": map[string]interface{}{"Power": 2000.0},
			"created_on":     map[string]interface{}{"secs_since_epoch": e[0]},
			"stage": map[string]interface{}{"S4": map[string]interface{}{"multi_configuration": map[string]interface{}{
				"measured_hashrate": map[string]interface{}{"GigaHashes": e[1]},
				"configurations": []interface{}{
					map[string]interface{}{"measured_hashrate": map[string]interface{}{"GigaHashes": e[2]}},
					map[string]interface{}{"measured_hashrate": map[string]interface{}{"GigaHashes": e[2]}},
				},
			}}},
		}
	}
	b, _ := json.Marshal(map[string]interface{}{"version": 9, "boards": boards})
	p := filepath.Join(t.TempDir(), "autotune.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func storedHR(t *testing.T, key string) (float64, float64) {
	b, _ := os.ReadFile(autotuneFile)
	var doc map[string]interface{}
	_ = json.Unmarshal(b, &doc)
	e := dig(doc, "boards", key)
	tot, _ := dig(e, "stage", "S4", "multi_configuration", "measured_hashrate", "GigaHashes").(float64)
	cfgs, _ := dig(e, "stage", "S4", "multi_configuration", "configurations").([]interface{})
	b0, _ := dig(cfgs[0], "measured_hashrate", "GigaHashes").(float64)
	return tot, b0
}

func TestStoredHashrate90OnceAndAfterResave(t *testing.T) {
	oldF, oldL, oldB := autotuneFile, loweredTunesFile, tuneBackupDir
	defer func() { autotuneFile, loweredTunesFile, tuneBackupDir = oldF, oldL, oldB }()
	autotuneFile = tuneDoc(t, map[string][3]float64{"2000W-x": {100, 76300, 25000}})
	loweredTunesFile = filepath.Join(t.TempDir(), "lowered.json")
	tuneBackupDir = t.TempDir()
	if !tunesPending() {
		t.Fatal("a new tune must be pending")
	}
	if ks, err := lowerStoredHashrate(); err != nil || len(ks) != 1 {
		t.Fatalf("first: %v %v", ks, err)
	}
	if tot, b0 := storedHR(t, "2000W-x"); math.Abs(tot-68670) > 0.01 || math.Abs(b0-22500) > 0.01 {
		t.Fatalf("90 %%: %v %v", tot, b0)
	}
	// Braiins reloads it and re-stamps created_on, same numbers: still done
	autotuneFile2 := tuneDoc(t, map[string][3]float64{"2000W-x": {150, 68670, 22500}})
	autotuneFile = autotuneFile2
	if tunesPending() {
		t.Fatal("a re-stamped (same hashrate) tune must not be pending")
	}
	// again: nothing (no 81 %)
	if ks, _ := lowerStoredHashrate(); len(ks) != 0 || tunesPending() {
		t.Fatalf("second call lowered %v", ks)
	}
	if tot, _ := storedHR(t, "2000W-x"); math.Abs(tot-68670) > 0.01 {
		t.Fatalf("lowered twice: %v", tot)
	}
	// Braiins retunes and re-saves (new created_on, its own fresh hashrate): lowered again
	autotuneFile = tuneDoc(t, map[string][3]float64{"2000W-x": {200, 75000, 25000}})
	if !tunesPending() {
		t.Fatal("a re-saved tune must be pending")
	}
	if ks, _ := lowerStoredHashrate(); len(ks) != 1 {
		t.Fatalf("re-save: %v", ks)
	}
	if tot, _ := storedHR(t, "2000W-x"); math.Abs(tot-67500) > 0.01 {
		t.Fatalf("re-save 90 %%: %v", tot)
	}
}

// Our own 90 % edit must not look like "Braiins changed its saved tune" to the profile guard.
func TestStoredHashrate90NotAProfileChange(t *testing.T) {
	a := map[string]interface{}{"stage": map[string]interface{}{"S4": map[string]interface{}{"multi_configuration": map[string]interface{}{
		"measured_hashrate": map[string]interface{}{"GigaHashes": 76300.0}, "voltage": 12021.0}}}}
	b := map[string]interface{}{"stage": map[string]interface{}{"S4": map[string]interface{}{"multi_configuration": map[string]interface{}{
		"measured_hashrate": map[string]interface{}{"GigaHashes": 68670.0}, "voltage": 12021.0}}}}
	ja, _ := json.Marshal(stripStamps(a))
	jb, _ := json.Marshal(stripStamps(b))
	if string(ja) != string(jb) {
		t.Fatalf("guard sees our edit as a change: %s vs %s", ja, jb)
	}
}
