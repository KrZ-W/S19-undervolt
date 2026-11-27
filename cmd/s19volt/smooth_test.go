package main

import (
	"math"
	"testing"
)

// The 2026-11-26 evening sweep (2 % steps, test profile 250 MHz): two one-step stalls in the raw readings
// (64 -> 66 %: 9.704 -> 9.676 V; 72 -> 74 %: 8.961 -> 8.963 V). The fit must smooth them out and stay close.
var sweep1126 = []calPoint{{Duty: 0, V: 11.803}, {Duty: 2, V: 11.799}, {Duty: 4, V: 11.806}, {Duty: 6, V: 11.798}, {Duty: 8, V: 11.8}, {Duty: 10, V: 11.805}, {Duty: 12, V: 11.805}, {Duty: 14, V: 11.8}, {Duty: 16, V: 11.807}, {Duty: 18, V: 11.801}, {Duty: 20, V: 11.803}, {Duty: 22, V: 11.801}, {Duty: 24, V: 11.795}, {Duty: 26, V: 11.798}, {Duty: 28, V: 11.792}, {Duty: 30, V: 11.78}, {Duty: 32, V: 11.768}, {Duty: 34, V: 11.745}, {Duty: 36, V: 11.702}, {Duty: 38, V: 11.641}, {Duty: 40, V: 11.593}, {Duty: 42, V: 11.515}, {Duty: 44, V: 11.404}, {Duty: 46, V: 11.284}, {Duty: 48, V: 11.205}, {Duty: 50, V: 11.038}, {Duty: 52, V: 10.881}, {Duty: 54, V: 10.676}, {Duty: 56, V: 10.576}, {Duty: 58, V: 10.431}, {Duty: 60, V: 10.261}, {Duty: 62, V: 10.001}, {Duty: 64, V: 9.704}, {Duty: 66, V: 9.676}, {Duty: 68, V: 9.507}, {Duty: 70, V: 9.246}, {Duty: 72, V: 8.961}, {Duty: 74, V: 8.963}, {Duty: 76, V: 8.845}, {Duty: 78, V: 8.829}, {Duty: 80, V: 8.822}, {Duty: 82, V: 8.822}, {Duty: 84, V: 8.82}, {Duty: 86, V: 8.806}}

func TestFitSmoothsStalls(t *testing.T) {
	f, ok := fitShape(sweep1126)
	if !ok {
		t.Fatal("no fit")
	}
	for d := 60.0; d < 72; d += 2 {
		if drop := f.at(d) - f.at(d+2); drop < 0.1 {
			t.Errorf("%.0f -> %.0f %%: fitted drop %.3f V (a stall left)", d, d+2, drop)
		}
	}
	var ss float64
	for _, p := range sweep1126 {
		ss += (f.at(p.Duty) - p.V) * (f.at(p.Duty) - p.V)
	}
	if sd := math.Sqrt(ss / float64(len(sweep1126))); sd > 0.04 {
		t.Errorf("fit too far from the points: rms %.3f V", sd)
	}
}
