package main

// metrics.go — bosminer's Prometheus endpoint (127.0.0.1:8081/metrics, refreshes ~10 s). The one reader:
// board stack voltages (/volts, UI, trim loop) and per-chip data (setup wizard: weak chips, chip map): the stock
// chip_hashrate gauge, or the chip_shares counters a patched bosminer adds.

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
)

var (
	stackRe = regexp.MustCompile(`hashboard_voltage\{hashboard="(\d)",type="feedback"\} (\S+)`)
	freqRe  = regexp.MustCompile(`chip_frequency\{chip_addr="(\d+)",chip_in_domain="\d+",hashboard="(\d)",voltage_domain="\d+"\} (\S+)`)
	shareRe = regexp.MustCompile(`chip_shares\{chip_addr="(\d+)",chip_in_domain="\d+",hashboard="(\d)",type="valid",voltage_domain="\d+"\} (\S+)`)
	hrateRe = regexp.MustCompile(`chip_hashrate\{chip_addr="(\d+)",chip_in_domain="\d+",hashboard="(\d)",type="valid",voltage_domain="\d+"\} (\S+)`)
)

const chipsPerBoard = 76 // BM1398: 38 voltage domains x 2; bosminer labels chips 0..75 (domain = index / 2)

func fetchMetrics() (string, error) {
	resp, err := httpc.Get("http://127.0.0.1:8081/metrics") // Go's client undoes bosminer's gzip
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// boardVolts: board -> stack voltage (a disabled board reads 0).
func boardVolts(m string) map[string]float64 {
	out := map[string]float64{}
	for _, x := range stackRe.FindAllStringSubmatch(m, -1) {
		if v, e := strconv.ParseFloat(x[2], 64); e == nil {
			out[x[1]] = v
		}
	}
	return out
}

// perChip: board -> 76 values of one per-chip metric (index = bosminer's chip label 0..75).
func perChip(re *regexp.Regexp, m string, scale float64) map[string][]float64 {
	out := map[string][]float64{}
	for _, x := range re.FindAllStringSubmatch(m, -1) {
		i, _ := strconv.Atoi(x[1])
		v, e := strconv.ParseFloat(x[3], 64)
		if e != nil || i < 0 || i >= chipsPerBoard {
			continue
		}
		if out[x[2]] == nil {
			out[x[2]] = make([]float64, chipsPerBoard)
		}
		out[x[2]][i] = v * scale
	}
	return out
}

// chipHashrate: board -> per-chip valid hashrate (stock bosminer metric, a gauge; units do not matter: weakChips
// compares chips with their board's median).
func chipHashrate(m string) map[string][]float64 { return perChip(hrateRe, m, 1) }

// chipMetrics: the per-chip parts of one scrape the wizard keeps (a few KB; the raw text is ~230 KB). ok = the
// scrape succeeded.
type chipMetrics struct {
	ok                   bool
	shares, hrate, freqs map[string][]float64
}

// parseChipMetrics: one scrape ("" = failed) -> its per-chip share counters, hashrate gauge and clocks.
func parseChipMetrics(m string) chipMetrics {
	return chipMetrics{ok: m != "", shares: chipShares(m), hrate: chipHashrate(m), freqs: chipFreqMHz(m)}
}

// chipRates: every chip's work over a window: valid-share counter deltas per minute between the window's first and
// last scrape when the (patched) chip_shares counters exist in both ("shares"), else the stock chip_hashrate gauge
// averaged over the window's samples (hr: one per ~30 s; validated offline against counted shares: ~1.1 % median,
// 3.7 % 90th pct on steady 5-min windows, weak-chip verdicts agreed), or the last scrape's gauge when there are no
// samples ("hashrate"). nil, "" when neither exists.
func chipRates(prev, cur chipMetrics, minutes float64, hr []map[string][]float64) (map[string][]float64, string) {
	if len(cur.shares) > 0 && len(prev.shares) > 0 {
		if r := shareRates(prev.shares, cur.shares, minutes); len(r) > 0 {
			return r, "shares"
		}
	}
	if m := meanChips(hr); len(m) > 0 {
		return m, "hashrate"
	}
	if len(cur.hrate) > 0 {
		return cur.hrate, "hashrate"
	}
	return nil, ""
}

// meanChips: per-chip mean of gauge samples (each board over the samples that have it). nil = none.
func meanChips(hr []map[string][]float64) map[string][]float64 {
	sum, n := map[string][]float64{}, map[string]int{}
	for _, h := range hr {
		for b, cs := range h {
			if sum[b] == nil {
				sum[b] = make([]float64, chipsPerBoard)
			}
			for i := 0; i < len(cs) && i < chipsPerBoard; i++ {
				sum[b][i] += cs[i]
			}
			n[b]++
		}
	}
	if len(sum) == 0 {
		return nil
	}
	for b := range sum {
		for i := range sum[b] {
			sum[b][i] /= float64(n[b])
		}
	}
	return sum
}

// mixChips: two consecutive windows' per-chip rates weighted wa / wb (a short check extended to the full hold);
// a chip unknown (< 0) on one side keeps the other side's; a board missing on one side keeps the other's.
func mixChips(a, b map[string][]float64, wa, wb float64) map[string][]float64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := map[string][]float64{}
	for bd, cb := range b {
		ca := a[bd]
		if ca == nil {
			out[bd] = cb
			continue
		}
		out[bd] = make([]float64, len(cb))
		for i := range cb {
			switch {
			case i >= len(ca) || ca[i] < 0:
				out[bd][i] = cb[i]
			case cb[i] < 0:
				out[bd][i] = ca[i]
			default:
				out[bd][i] = ca[i]*wa + cb[i]*wb
			}
		}
	}
	for bd, ca := range a {
		if _, ok := b[bd]; !ok {
			out[bd] = ca
		}
	}
	return out
}

// chipShares: board -> per-chip valid-share counter (index = bosminer's chip label 0..75). Counters are cumulative;
// the difference between two readings is that chip's work in between (mcr is a ~30 min window, useless per step).
func chipShares(m string) map[string][]float64 { return perChip(shareRe, m, 1) }

// shareRates: per-chip valid shares per minute between two counter readings (a counter that went
// backwards — restart or 32-bit wrap — gives -1 = unknown).
func shareRates(a, b map[string][]float64, minutes float64) map[string][]float64 {
	out := map[string][]float64{}
	for bd, cb := range b {
		ca := a[bd]
		if ca == nil || minutes <= 0 {
			continue
		}
		out[bd] = make([]float64, chipsPerBoard)
		for i := range cb {
			if d := cb[i] - ca[i]; d >= 0 {
				out[bd][i] = math.Round(d/minutes*10) / 10
			} else {
				out[bd][i] = -1
			}
		}
	}
	return out
}

// chipFreqMHz: board -> per-chip clock in MHz (stock metric).
func chipFreqMHz(m string) map[string][]float64 { return perChip(freqRe, m, 1e-6) }

// weakChips: chips whose work per MHz is below thresh x their board's median (the floor search's definition:
// a chip doing < 60 % of what its neighbours do per MHz is weak). rates = per-chip work (chipRates, either source:
// only ratios within a board matter), freqs in MHz.
func weakChips(rates, freqs map[string][]float64, thresh float64) map[string]bool {
	out := map[string]bool{}
	for b, rs := range rates {
		fs := freqs[b]
		per := []float64{}
		val := make([]float64, len(rs))
		for i, r := range rs {
			if r < 0 || fs == nil || fs[i] <= 0 {
				val[i] = -1
				continue
			}
			val[i] = r / fs[i]
			per = append(per, val[i])
		}
		if len(per) == 0 {
			continue
		}
		sort.Float64s(per)
		med := per[len(per)/2]
		for i, v := range val {
			if v >= 0 && med > 0 && v/med < thresh {
				out[fmt.Sprintf("%s/%d", b, i)] = true
			}
		}
	}
	return out
}

// stepMCR: a hold's own hashrate from the per-chip data vs the same level's stock baseline (both from the same
// source; NOT bosminer's 30-min chip_mcr): total and per-board ratios over the chips known (>= 0) in both windows.
// ok = false when either window has no per-chip data or the sources differ.
func stepMCR(step, base map[string][]float64, stepSrc, baseSrc string) (float64, map[string]float64, bool) {
	if step == nil || base == nil || stepSrc != baseSrc {
		return 0, nil, false
	}
	per := map[string]float64{}
	ts, tb := 0.0, 0.0
	for b, cb := range base {
		cs := step[b]
		s, bb := 0.0, 0.0
		for i := range cb {
			if i < len(cs) && cs[i] >= 0 && cb[i] >= 0 {
				s, bb = s+cs[i], bb+cb[i]
			}
		}
		if bb > 0 {
			per[b] = math.Round(s/bb*1000) / 1000
			ts, tb = ts+s, tb+bb
		}
	}
	if tb <= 0 {
		return 0, nil, false
	}
	return math.Round(ts/tb*1000) / 1000, per, true
}

// sharesTHs: per-chip valid shares per minute (difficulty-1 shares, 2^32 hashes each) -> TH/s (unknown chips skipped).
func sharesTHs(chips map[string][]float64) float64 {
	n := 0.0
	for _, cs := range chips {
		for _, c := range cs {
			if c > 0 {
				n += c
			}
		}
	}
	return math.Round(n*math.Pow(2, 32)/60/1e12*100) / 100
}
