package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Uses a copy of a real bosminer-autotune.json when S19_AUTOTUNE_COPY points at one (never the miner's file).
func TestScaleProfileClocks(t *testing.T) {
	src := os.Getenv("S19_AUTOTUNE_COPY")
	if src == "" {
		t.Skip("set S19_AUTOTUNE_COPY to a copy of /etc/bosminer-autotune.json")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	autotuneFile = filepath.Join(dir, "autotune.json")
	if err := os.WriteFile(autotuneFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	freq := func(path string, target int) (sum float64, n int) {
		var d map[string]interface{}
		raw, _ := os.ReadFile(path)
		_ = json.Unmarshal(raw, &d)
		for _, v := range d["boards"].(map[string]interface{}) {
			e := v.(map[string]interface{})
			if int(e["profile_target"].(map[string]interface{})["Power"].(float64)) != target {
				continue
			}
			for _, c := range dig(e, "stage", "S4", "multi_configuration", "configurations").([]interface{}) {
				for _, ch := range dig(c, "params", "chip_params_set", "inner").([]interface{}) {
					for _, x := range ch.(map[string]interface{})["pll_frequencies"].([]interface{}) {
						sum += x.(float64)
						n++
					}
				}
			}
		}
		return
	}
	before, nb := freq(autotuneFile, 1000)
	other0, _ := freq(autotuneFile, 1100)
	n, err := scaleProfileClocks(1000, 3, 0.95)
	if err != nil || n != nb || n != 228 {
		t.Fatalf("scaled %d chips (want %d = 228), err %v", n, nb, err)
	}
	after, _ := freq(autotuneFile, 1000)
	other1, _ := freq(autotuneFile, 1100)
	if math.Abs(after/before-0.95) > 1e-6 || other0 != other1 {
		t.Fatalf("ratio %.6f, other target changed: %v", after/before, other0 != other1)
	}
	if _, err := scaleProfileClocks(1234, 3, 0.95); err == nil {
		t.Fatal("missing target must fail")
	}
}
