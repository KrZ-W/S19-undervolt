package main

import (
	"math"
	"testing"
)

const sampleMetrics = `# HELP chip_mcr per-chip
chip_mcr{chip_addr="000",chip_in_domain="00",hashboard="1",voltage_domain="00"} 0.9850447609951846 1795579702771
chip_mcr{chip_addr="003",chip_in_domain="01",hashboard="1",voltage_domain="00"} 0.5 1795579702771
chip_mcr{chip_addr="075",chip_in_domain="01",hashboard="3",voltage_domain="37"} 1.0080023527207063 1795579702771
chip_shares{chip_addr="000",chip_in_domain="00",hashboard="1",type="valid",voltage_domain="00"} 101808 1795579702771
chip_shares{chip_addr="000",chip_in_domain="00",hashboard="1",type="error",voltage_domain="00"} 7 1795579702771
chip_shares{chip_addr="075",chip_in_domain="01",hashboard="3",type="valid",voltage_domain="37"} 9000 1795579702771
hashboard_voltage{hashboard="1",type="feedback"} 10.1764 1795579702771
hashboard_voltage{hashboard="2",type="feedback"} 0 1795579702771
`

func TestMetricsParsing(t *testing.T) {
	v := boardVolts(sampleMetrics)
	if v["1"] != 10.1764 || v["2"] != 0 || len(v) != 2 {
		t.Fatalf("boardVolts: %v", v)
	}
}

func TestShareRates(t *testing.T) {
	a := chipShares(sampleMetrics)
	if a["1"][0] != 101808 || a["3"][75] != 9000 {
		t.Fatalf("chipShares valid only: %v", a["1"][0])
	}
	b := map[string][]float64{"1": append([]float64(nil), a["1"]...)}
	b["1"][0] += 250
	b["1"][1] = -5 // counter went backwards
	r := shareRates(a, b, 5)
	if r["1"][0] != 50 || r["1"][1] != -1 {
		t.Fatalf("shareRates: %v %v", r["1"][0], r["1"][1])
	}
}

func TestWeakChips(t *testing.T) {
	r := map[string][]float64{"1": make([]float64, chipsPerBoard)}
	f := map[string][]float64{"1": make([]float64, chipsPerBoard)}
	for i := range r["1"] {
		r["1"][i], f["1"][i] = 1700, 200
	}
	r["1"][36] = 900                  // 53 % of the median per MHz -> weak
	r["1"][37] = 1100                 // 65 % -> not weak
	f["1"][38], r["1"][38] = 100, 850 // half the clock, half the work -> same per MHz -> not weak
	r["1"][39] = -1                   // unknown counter -> ignored
	w := weakChips(r, f, 0.6)
	if len(w) != 1 || !w["1/36"] {
		t.Fatalf("weak = %v, want only 1/36", w)
	}
}

func TestStepMCR(t *testing.T) {
	base := map[string][]float64{"1": {10, 10, 10}, "2": {10, 10, -1}}
	step := map[string][]float64{"1": {9, 10, -1}, "2": {10, 9, 50}}
	m, per, ok := stepMCR(step, base, "shares", "shares")
	// board 1: chips 0-1 known in both (19/20); board 2: chips 0-1 (19/20); total 38/40
	if !ok || m != 0.95 || per["1"] != 0.95 || per["2"] != 0.95 {
		t.Fatalf("%v %v %v", m, per, ok)
	}
	if _, _, ok := stepMCR(step, base, "hashrate", "shares"); ok {
		t.Fatal("different sources must not compare")
	}
	if _, _, ok := stepMCR(nil, base, "shares", "shares"); ok {
		t.Fatal("no per-chip data")
	}
	if th := sharesTHs(map[string][]float64{"1": {1e6, 1e6, -1}}); math.Abs(th-143.17) > 0.01 {
		t.Fatalf("2e6 shares/min: %v TH/s", th)
	}
}
