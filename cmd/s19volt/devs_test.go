package main

import (
	"fmt"
	"testing"
	"time"
)

// devsReply: a cgminer-API devs reply (bosminer style: ID per board, numbers as JSON numbers, trailing NUL trimmed
// by cgCommand). mh = Total MH per board 1..3, el = Device Elapsed.
func devsReply(el int, mh ...float64) []byte {
	s := `{"STATUS":[{"STATUS":"S","Msg":"3 ASC(s)"}],"DEVS":[`
	for i, m := range mh {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`{"ASC":%d,"Name":"","ID":%d,"Enabled":"Y","Status":"Alive","MHS 1m":1.0,"Total MH":%v,"Device Elapsed":%d}`, i, i+1, m, el)
	}
	return []byte(s + `],"id":1}`)
}

func TestExactRatesWindow(t *testing.T) {
	t0 := time.Unix(1e9, 0)
	a := parseDevs(devsReply(1000, 1e12, 2e12, 3e12), t0)
	// 300 s later: board 1 did 30 TH/s (9e9 MH), board 2 31.5, board 3 29
	b := parseDevs(devsReply(1300, 1e12+9e9, 2e12+9.45e9, 3e12+8.7e9), t0.Add(300*time.Second))
	if !a.ok || !b.ok || len(a.devs) != 3 {
		t.Fatalf("parse: %+v", a)
	}
	fb := map[string]float64{"1": 29.5, "2": 31, "3": 29.4} // board rates of the same window (1-min means)
	rates, tot, src, err := exactRates(a, b, fb, 89.9)
	if err != nil || src != "devs" || rates["1"] != 30 || rates["2"] != 31.5 || rates["3"] != 29 || tot != 90.5 {
		t.Fatalf("window: %v %v %s %v", rates, tot, src, err)
	}
	// a disabled board is not in the measured set: only the enabled ones are reported
	rates, tot, _, _ = exactRates(a, b, map[string]float64{"1": 30, "3": 29}, 59)
	if len(rates) != 2 || tot != 59 {
		t.Fatalf("subset: %v %v", rates, tot)
	}
}

func TestExactRatesCounterReset(t *testing.T) {
	t0 := time.Unix(1e9, 0)
	a := parseDevs(devsReply(5000, 1e12, 2e12, 3e12), t0)
	// bosminer restarted 100 s ago: counters small again
	b := parseDevs(devsReply(100, 3e9, 3e9, 3e9), t0.Add(300*time.Second))
	fb := map[string]float64{"1": 30, "2": 30, "3": 30}
	rates, tot, src, err := exactRates(a, b, fb, 90)
	if !counterReset(err) || src != "boards" || tot != 90 || rates["1"] != 30 {
		t.Fatalf("reset: %v %v %s %v", rates, tot, src, err)
	}
	// Device Elapsed going backwards alone (counters happen to be higher) is a restart too
	b = parseDevs(devsReply(100, 2e12, 3e12, 4e12), t0.Add(300*time.Second))
	if _, _, _, err := exactRates(a, b, fb, 90); !counterReset(err) {
		t.Fatalf("elapsed backwards: %v", err)
	}
}

func TestExactRatesFallback(t *testing.T) {
	t0 := time.Unix(1e9, 0)
	a := parseDevs(devsReply(1000, 1e12, 2e12), t0)
	b := parseDevs(devsReply(1300, 1e12+9e9, 2e12+9e9), t0.Add(300*time.Second))
	fb := map[string]float64{"1": 29.8, "2": 30.1}
	for name, c := range map[string][2]devsSnap{
		"port 4028 unavailable": {{}, b},
		"no time":               {b, b},
	} {
		rates, tot, src, err := exactRates(c[0], c[1], fb, 59.9)
		if err != nil || src != "boards" || tot != 59.9 || rates["2"] != 30.1 {
			t.Fatalf("%s: %v %v %s %v", name, rates, tot, src, err)
		}
	}
	// an unreadable reply is !ok
	if parseDevs([]byte(`{"STATUS":[{"STATUS":"E"}]}`), t0).ok || parseDevs([]byte(`{"DEVS":[{"ASC":0}]}`), t0).ok {
		t.Fatal("reply without Total MH must not parse")
	}
	// a rate far off the board rate (wrong mapping / unit) is not trusted
	if _, _, src, _ := exactRates(a, b, map[string]float64{"1": 15, "2": 30}, 45); src != "boards" {
		t.Fatalf("implausible: %s", src)
	}
}

func TestDevsBoardMapping(t *testing.T) {
	// IDs not the REST board ids (here 6,7,8): enabled entries in ASC order onto the sorted board ids
	raw := []byte(`{"DEVS":[{"ASC":1,"ID":7,"Enabled":"Y","Total MH":200,"Device Elapsed":5},
{"ASC":0,"ID":6,"Enabled":"Y","Total MH":100,"Device Elapsed":5},
{"ASC":2,"ID":8,"Enabled":"N","Total MH":0,"Device Elapsed":5}]}`)
	m := parseDevs(raw, time.Unix(1, 0)).byBoard([]string{"3", "1"})
	if m == nil || m["1"].mh != 100 || m["3"].mh != 200 {
		t.Fatalf("ASC order: %+v", m)
	}
	if parseDevs(raw, time.Unix(1, 0)).byBoard([]string{"1", "2", "3"}) != nil {
		t.Fatal("count mismatch must not map")
	}
	// IDs = board ids: matched by ID whatever the order
	m = parseDevs(devsReply(5, 10, 20, 30), time.Unix(1, 0)).byBoard([]string{"2", "3"})
	if m["2"].mh != 20 || m["3"].mh != 30 {
		t.Fatalf("by id: %+v", m)
	}
}

// the per-chip source: chip_shares deltas when the patched counter exists in both ends, else the 30-s chip_hashrate
// samples averaged, else the last scrape's gauge.
func TestChipRatesSourceSelection(t *testing.T) {
	m0 := chipMetrics{ok: true, hrate: map[string][]float64{"1": {9}}}
	m1 := chipMetrics{ok: true, hrate: map[string][]float64{"1": {4}}}
	hr := []map[string][]float64{{"1": {1, 10}}, {"1": {3, 20}}, {"1": {5, 30}}}
	r, src := chipRates(m0, m1, 5, hr)
	if src != "hashrate" || r["1"][0] != 3 || r["1"][1] != 20 {
		t.Fatalf("stock: mean of the samples: %v %s", r["1"][:2], src)
	}
	if r, src = chipRates(m0, m1, 5, nil); src != "hashrate" || r["1"][0] != 4 {
		t.Fatalf("no samples: last gauge: %v %s", r["1"][:1], src)
	}
	m0.shares, m1.shares = map[string][]float64{"1": {100}}, map[string][]float64{"1": {600}}
	if r, src = chipRates(m0, m1, 5, hr); src != "shares" || r["1"][0] != 100 {
		t.Fatalf("patched: shares win: %v %s", r["1"][:1], src)
	}
	m0.shares = nil // counter only at one end: the stock source
	if _, src = chipRates(m0, m1, 5, hr); src != "hashrate" {
		t.Fatalf("one-sided shares: %s", src)
	}
	// weak chips work on the hashrate source (ratios within a board): chip 1 at 30 % of the median
	rates := map[string][]float64{"1": {10, 3, 10, 11}}
	w := weakChips(rates, map[string][]float64{"1": {500, 500, 500, 500}}, weakFr)
	if !w["1/1"] || len(w) != 1 {
		t.Fatalf("weak: %v", w)
	}
	// extension: two windows mixed by time
	mx := mixChips(map[string][]float64{"1": {10, -1}}, map[string][]float64{"1": {20, 8}, "2": {1}}, 0.4, 0.6)
	if mx["1"][0] != 16 || mx["1"][1] != 8 || mx["2"][0] != 1 {
		t.Fatalf("mix: %v", mx)
	}
}

// the stock baseline window from the sliding buffer: Total MH between the anchor and the newest sample, per-chip
// from the chip_hashrate samples after the anchor (stock: no chip_shares).
func TestSummarizeBaselineStock(t *testing.T) {
	hold := 5 * time.Minute
	t0 := time.Unix(1e9, 0)
	var w baseWindow
	w.restart(t0)
	for sec := 0; sec <= 450; sec += 15 {
		at := t0.Add(time.Duration(sec) * time.Second)
		mh := float64(sec) * 30e6 // 30 TH/s
		w.add(rateSample{at: at,
			r:  reading{ok: true, mining: true, rates: map[string]float64{"1": 29}, chip: 60, volts: 12},
			m:  chipMetrics{ok: true, hrate: map[string][]float64{"1": {float64(sec)}}, freqs: map[string][]float64{"1": {500}}},
			dv: parseDevs(devsReply(sec, mh), at)}, hold)
	}
	mw, ok := w.last(hold)
	if !ok {
		t.Fatal("not ready")
	}
	rates, tot, _, _, err := summarize(mw)
	if err != nil || lastRateSrc != "devs" || rates["1"] != 30 || tot != 30 {
		t.Fatalf("baseline: %v %v %s %v", rates, tot, lastRateSrc, err)
	}
	// anchor 150 s (the newest sample 5 min before 450 s): samples 165..450 s, mean 307.5
	if lastChipSrc != "hashrate" || lastChips["1"][0] != 307.5 {
		t.Fatalf("chips: %v %s", lastChips["1"][:1], lastChipSrc)
	}
}

// devsReplyD1: like devsReply, with "Diff1 Work" per board (d1) next to Total MH (mh).
func devsReplyD1(el []int, mh, d1 []float64) []byte {
	s := `{"STATUS":[{"STATUS":"S","Msg":"3 ASC(s)"}],"DEVS":[`
	for i := range mh {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`{"ASC":%d,"Name":"","ID":%d,"Enabled":"Y","Status":"Alive","Total MH":%v,"Diff1 Work":%v,"Device Elapsed":%d}`,
			i, i+1, mh[i], d1[i], el[i])
	}
	return []byte(s + `],"id":1}`)
}

// Two real reads 30 s apart (S19, 2000 W, 2026-11-26): Total MH says 56.9 / 19.9 / 58.0 TH/s (its lumps), Diff1
// Work says ~23.9 / 23.0 / 24.3 TH/s, in line with bosminer's own 1-min rates (24.4 / 23.7 / 24.8). The window must
// use Diff1 Work.
func TestExactRatesDiff1RealLumps(t *testing.T) {
	t0 := time.Unix(1795700914, 0)
	a := parseDevs(devsReplyD1([]int{4957, 4956, 4954},
		[]float64{118316247241.7198, 101295807243.74733, 108781282405.64633}, []float64{100464, 98169, 99190}), t0)
	b := parseDevs(devsReplyD1([]int{4987, 4986, 4984},
		[]float64{120022689288.02815, 101893941569.25746, 110522908824.04352}, []float64{101114, 98798, 99852}),
		t0.Add(30*time.Second))
	fb := map[string]float64{"1": 24.4, "2": 23.7, "3": 24.8}
	rates, tot, src, err := exactRates(a, b, fb, 72.9)
	if err != nil || src != "devs" {
		t.Fatalf("src %s err %v", src, err)
	}
	for bd, want := range map[string]float64{"1": 23.82, "2": 23.05, "3": 24.27} {
		if d := rates[bd] - want; d > 0.05 || d < -0.05 {
			t.Fatalf("board %s: %.3f want %.2f (%v)", bd, rates[bd], want, rates)
		}
	}
	if tot < 71 || tot > 71.3 {
		t.Fatalf("total %.3f", tot)
	}
}

// Diff1 Work going backwards = bosminer restarted under the window, like Total MH.
func TestExactRatesDiff1Reset(t *testing.T) {
	t0 := time.Unix(1e9, 0)
	a := parseDevs(devsReplyD1([]int{5000, 5000, 5000}, []float64{1e12, 1e12, 1e12}, []float64{9e4, 9e4, 9e4}), t0)
	b := parseDevs(devsReplyD1([]int{5300, 5300, 5300}, []float64{1.01e12, 1.01e12, 1.01e12}, []float64{9.6548e4, 50, 9.6548e4}), t0.Add(300*time.Second))
	if _, _, _, err := exactRates(a, b, map[string]float64{"1": 24, "2": 24, "3": 24}, 72); !counterReset(err) {
		t.Fatalf("diff1 backwards: %v", err)
	}
}
