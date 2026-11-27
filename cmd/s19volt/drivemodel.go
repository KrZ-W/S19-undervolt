package main

// drivemodel.go — the fitted drive model V(drive, load): the one answer to "which drive % gives board voltage v at
// load T" whenever a sweep curve exists (startDrive, and through it driveStep's local slope and the drive chart).
//
//	V(d, T) = S(d + δ + k·(T − Ts))
//
// S  the sweep's shape: the 0-100 % sweep points (at the test profile, load Ts) sorted by drive, a 3-point median
//    (one wild read does not drag the fit), isotonic regression to a non-increasing sequence (PAVA; pooled and
//    equal neighbours become one knot at their centre: a stale duplicate read of the 2 %/4 s sweep counts once, half
//    a step earlier; the first and last blocks — the 0 % plateau and the floor — keep their inner edge instead), then a monotone piecewise-cubic (PCHIP, Fritsch-Carlson) through the
//    knots, flat outside them. PCHIP keeps the measured shape (the sharp knee into the floor) where a fitted
//    logistic / Richards curve leaves 0.05-0.07 V systematic misfit in the knee and around 50-60 %; on the first
//    real sweep (2026-11-25) PCHIP gave the smallest residuals and the best step prediction.
// δ  a constant drive offset between the sweep and the held steps (the sweep's read lag, the PSU's slow settle);
// k  the drive shift per watt of load (the PSU's output line moves with load: a higher bosminer target needs a
//    little LESS drive for the same voltage).
//
// δ and k come from the measured search steps (hold means) and the preparation's droop points (droop.go: two loads,
// three drives each, weighed as a full hold; they give k before any level ran), by weighted least squares in drive space: each step
// gives u = S⁻¹(V) − d (exact: the model is linear in δ and k there), weight 1 for a full hold, shortW for a short
// check, and a ridge prior pulls k toward 0 (kRidge): steps at one load only give a constant offset (k = 0), no
// step gives the sweep's shape alone. A step whose voltage is on S's flat ends says nothing about the shift and is
// skipped. Horizontal shift beat a vertical offset (S(d) + a + b·(T − Ts)) on the first real data: leave-one-out
// drive error 0.54 vs 0.59 %, and 58.7 vs 58.9 % for the reference miss (measured 58.6 %).

import (
	"math"
	"sort"
)

const (
	shortW  = 0.4    // fit weight of a short-check step (a full hold = 1)
	kRidge  = 2500.0 // ridge on k, in weight·W²: one full hold 50 W away from the rest moves k as much as the prior holds it
	flatTol = 0.03   // V: a step within this of S's plateau or floor is on a flat end (no shift information)
)

// shapeFit: S(d), monotone non-increasing, flat outside [x[0], x[n-1]].
type shapeFit struct {
	x, y, m []float64 // knots and PCHIP slopes
}

// fitShape: S from sweep points (false = fewer than 2 distinct drives).
func fitShape(pts []calPoint) (shapeFit, bool) {
	type xy struct{ x, y float64 }
	var ps []xy
	for _, p := range pts {
		if p.V > 0 {
			ps = append(ps, xy{p.Duty, p.V})
		}
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].x < ps[j].x })
	var xs, ys []float64 // equal drives averaged
	for i := 0; i < len(ps); {
		j, s := i, 0.0
		for ; j < len(ps) && ps[j].x == ps[i].x; j++ {
			s += ps[j].y
		}
		xs, ys = append(xs, ps[i].x), append(ys, s/float64(j-i))
		i = j
	}
	if len(xs) < 2 {
		return shapeFit{}, false
	}
	// Smoothing: a sliding quadratic least-squares fit over smoothHalf points each side (owner 2026-11-26: single
	// stale or noisy readings left one-step stalls in the curve — 9.70 -> 9.68 V and 8.963 -> 8.961 V on a 2 %
	// sweep — that the old 3-point median kept). On that sweep it gives even steps (~-0.2 V per 2 %), keeps the
	// knee into the floor, residual sd 0.027 V. The monotone pass below then removes any wiggle left.
	med := smoothQuad(xs, ys, smoothHalfV)
	// PAVA (non-increasing): blocks of (mean, count, drive sum)
	type blk struct{ v, n, xs float64 }
	var bs []blk
	for i := range med {
		bs = append(bs, blk{med[i], 1, xs[i]})
		for len(bs) > 1 && bs[len(bs)-2].v <= bs[len(bs)-1].v { // equal reads pooled too: a stale duplicate is one reading
			a, b := bs[len(bs)-2], bs[len(bs)-1]
			bs = append(bs[:len(bs)-2], blk{(a.v*a.n + b.v*b.n) / (a.n + b.n), a.n + b.n, a.xs + b.xs})
		}
	}
	var f shapeFit
	for i, b := range bs { // the end blocks (plateau, floor) keep their inner edge: the knee stays where it was read
		x := b.xs / b.n
		if i == 0 && len(bs) > 1 {
			x = xs[int(b.n)-1]
		} else if i == len(bs)-1 && i > 0 {
			x = xs[len(xs)-int(b.n)]
		}
		f.x, f.y = append(f.x, x), append(f.y, b.v)
	}
	if len(f.x) < 2 {
		f.x, f.y = []float64{f.x[0], f.x[0] + 1}, []float64{f.y[0], f.y[0]} // one block: flat
	}
	f.m = pchipSlopes(f.x, f.y)
	return f, true
}

// pchipSlopes: Fritsch-Carlson slopes (as scipy's PchipInterpolator): monotone data stays monotone.
func pchipSlopes(x, y []float64) []float64 {
	n := len(x)
	h, dl := make([]float64, n-1), make([]float64, n-1)
	for i := 0; i < n-1; i++ {
		h[i], dl[i] = x[i+1]-x[i], (y[i+1]-y[i])/(x[i+1]-x[i])
	}
	m := make([]float64, n)
	if n == 2 {
		m[0], m[1] = dl[0], dl[0]
		return m
	}
	for i := 1; i < n-1; i++ {
		if dl[i-1]*dl[i] <= 0 {
			continue
		}
		w1, w2 := 2*h[i]+h[i-1], h[i]+2*h[i-1]
		m[i] = (w1 + w2) / (w1/dl[i-1] + w2/dl[i])
	}
	end := func(h0, h1, d0, d1 float64) float64 {
		s := ((2*h0+h1)*d0 - h0*d1) / (h0 + h1)
		switch {
		case math.Signbit(s) != math.Signbit(d0) || d0 == 0:
			return 0
		case math.Signbit(d0) != math.Signbit(d1) && math.Abs(s) > 3*math.Abs(d0):
			return 3 * d0
		}
		return s
	}
	m[0] = end(h[0], h[1], dl[0], dl[1])
	m[n-1] = end(h[n-2], h[n-3], dl[n-2], dl[n-3])
	return m
}

// at: S(d).
func (f shapeFit) at(d float64) float64 {
	n := len(f.x)
	if d <= f.x[0] {
		return f.y[0]
	}
	if d >= f.x[n-1] {
		return f.y[n-1]
	}
	i := sort.SearchFloat64s(f.x, d) - 1
	h := f.x[i+1] - f.x[i]
	t := (d - f.x[i]) / h
	t2, t3 := t*t, t*t*t
	return (2*t3-3*t2+1)*f.y[i] + (t3-2*t2+t)*h*f.m[i] + (-2*t3+3*t2)*f.y[i+1] + (t3-t2)*h*f.m[i+1]
}

// inv: the least drive in [lo, hi] where S reaches v (S is non-increasing); false = v below S(hi).
func (f shapeFit) inv(v, lo, hi float64) (float64, bool) {
	if f.at(lo) <= v {
		return lo, true
	}
	if f.at(hi) > v {
		return hi, false
	}
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if f.at(mid) <= v {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, true
}

// driveModel: V(d, T) = S(d + Delta + K·(T − Ts)).
type driveModel struct {
	S     shapeFit
	Ts    float64 // the sweep's load
	Delta float64 // drive offset of the steps vs the sweep (%)
	K     float64 // drive shift per W of load (%/W)
	N     int     // step and droop points in the fit
	ND    int     // ... of which droop points
}

// shift: the drive offset at load T.
func (m driveModel) shift(T float64) float64 { return m.Delta + m.K*(T-m.Ts) }

// V: the model's board voltage at drive d, load T.
func (m driveModel) V(d, T float64) float64 { return m.S.at(d + m.shift(T)) }

// Drive: the drive % for voltage v at load T (not clamped; false = v below the sweep's floor: the deepest drive).
func (m driveModel) Drive(v, T float64) (float64, bool) {
	s := m.shift(T)
	e, ok := m.S.inv(v, s, 100+s) // model coordinate e = d + shift; d in 0..100
	return e - s, ok
}

// newDriveModel: the model from one sweep curve and the measured step points to fit δ and k on (callers choose
// them: startDrive prefers the same board set). False = no usable curve.
func newDriveModel(c wizCurve, steps []drivePt) (driveModel, bool) {
	S, ok := fitShape(c.Points)
	if !ok {
		return driveModel{}, false
	}
	m := driveModel{S: S, Ts: float64(c.Watts)}
	top, bot := S.y[0], S.y[len(S.y)-1]
	lo, hi := S.x[0], S.x[len(S.x)-1]
	var sw, swx, swxx, swy, swxy float64
	for _, p := range steps {
		if (p.Src != "step" && p.Src != "droop") || p.V <= 0 || p.V >= top-flatTol || p.V <= bot+flatTol {
			continue
		}
		e, _ := S.inv(p.V, lo, hi)
		w, x, u := 1.0, float64(p.load())-m.Ts, e-p.D // a droop point weighs as a full hold
		if p.Short {
			w = shortW
		}
		sw, swx, swxx, swy, swxy = sw+w, swx+w*x, swxx+w*x*x, swy+w*u, swxy+w*x*u
		m.N++
		if p.Src == "droop" {
			m.ND++
		}
	}
	if m.N == 0 {
		return m, true
	}
	a, b, d := sw, swx, swxx+kRidge // [a b; b d]·[δ k] = [swy swxy]
	det := a*d - b*b
	m.Delta, m.K = (swy*d-b*swxy)/det, (a*swxy-b*swy)/det
	return m, true
}

// modelFor: the drive model for load w and boards b, or false (no curve with points). The curve nearest in load
// (the wizard runs one sweep); the droop points (droop.go, every board) always, plus the steps of the same board set
// when any are usable, else every step.
func modelFor(curves []wizCurve, pts []drivePt, w int, b string) (driveModel, bool) {
	ci := -1
	for i, c := range curves {
		if len(c.Points) < 2 {
			continue
		}
		if ci < 0 || abs(c.Watts-w) <= abs(curves[ci].Watts-w) {
			ci = i
		}
	}
	if ci < 0 {
		return driveModel{}, false
	}
	var same []drivePt
	for _, p := range pts {
		if p.Src == "droop" || (p.Src == "step" && p.B == b) {
			same = append(same, p)
		}
	}
	m, ok := newDriveModel(curves[ci], same)
	if ok && m.N == m.ND {
		m, ok = newDriveModel(curves[ci], pts)
	}
	return m, ok
}

// driveFit: one fitted line on the drive chart (a level's load and board set).
type driveFit struct {
	L     int          `json:"l"`
	W     int          `json:"w"`
	B     string       `json:"b"`
	Delta float64      `json:"delta"`
	K     float64      `json:"k"`
	N     int          `json:"n"`
	Pts   [][2]float64 `json:"pts"` // [drive %, V]
}

// driveFits: the model's curve for every level that has measured steps or is running (the drive chart).
func driveFits(s wizState) []driveFit {
	var out []driveFit
	for _, lv := range s.Levels {
		w := 0
		for _, p := range s.Points {
			if p.Src == "step" && p.L == lv.Index {
				w = p.W
			}
		}
		if w == 0 && lv.Status == "running" {
			w = lv.Target
		}
		if w == 0 {
			continue
		}
		m, ok := modelFor(s.Curves, s.Points, w, lv.Boards)
		if !ok {
			return nil
		}
		f := driveFit{L: lv.Index, W: w, B: lv.Boards, Delta: math.Round(m.Delta*1000) / 1000, K: math.Round(m.K*1e5) / 1e5, N: m.N}
		for d := 0.0; d <= 100; d++ {
			f.Pts = append(f.Pts, [2]float64{d, math.Round(m.V(d, float64(w))*1000) / 1000})
		}
		out = append(out, f)
	}
	return out
}

// smoothHalf: points each side in the sliding quadratic fit (7-point window on a 2 % sweep = +/-6 % drive).
const smoothHalf = 3

// smoothHalfV: the half-window in use (a var so tests can compare; 0 = 3-point median, the old fit).
var smoothHalfV = smoothHalf

// smoothQuad: each y replaced by the value at its x of a least-squares quadratic through the points within h
// each side (fewer at the ends; fewer than 3 points or a singular fit = the value is kept).
func smoothQuad(xs, ys []float64, h int) []float64 {
	out := append([]float64(nil), ys...)
	if h == 0 { // the old 3-point median
		for i := 1; i+1 < len(ys); i++ {
			a := []float64{ys[i-1], ys[i], ys[i+1]}
			sort.Float64s(a)
			out[i] = a[1]
		}
		return out
	}
	for i := range xs {
		lo, hi := i-h, i+h
		if lo < 0 {
			lo = 0
		}
		if hi > len(xs)-1 {
			hi = len(xs) - 1
		}
		if hi-lo+1 < 3 {
			continue
		}
		// normal equations for y = a + b·u + c·u², u = x - xs[i] (centred: the value at xs[i] is a)
		var s [5]float64
		var t [3]float64
		for j := lo; j <= hi; j++ {
			u := xs[j] - xs[i]
			p := 1.0
			for k := 0; k < 5; k++ {
				s[k] += p
				if k < 3 {
					t[k] += p * ys[j]
				}
				p *= u
			}
		}
		m := [3][3]float64{{s[0], s[1], s[2]}, {s[1], s[2], s[3]}, {s[2], s[3], s[4]}}
		det := func(m [3][3]float64) float64 {
			return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) - m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) + m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
		}
		d := det(m)
		if math.Abs(d) < 1e-12 {
			continue
		}
		ma := m
		for r := 0; r < 3; r++ {
			ma[r][0] = t[r]
		}
		out[i] = det(ma) / d // Cramer: the constant term a
	}
	return out
}
