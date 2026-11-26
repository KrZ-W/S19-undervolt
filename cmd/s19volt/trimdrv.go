package main

// PSU trim driver (V2, 2026-11-24): the control board drives the
// optocoupler from the empty 4th hashboard connector's reset line with software PWM (package ch3).
// The trim loop, glides, current limit and level changes only ever call drv.Set / Target / Applied.

import (
	"fmt"
	"time"

	"s19psu/ch3"
)

type trimDriver struct{ p *ch3.PWM }

func (trimDriver) Name() string                { return "gpio (4th connector reset line)" }
func (t trimDriver) Set(pct float64) error     { t.p.Set(pct); return nil }
func (t trimDriver) SetNow(pct float64)        { t.p.SetNow(pct) }
func (t trimDriver) Target() (float64, error)  { return t.p.Target(), nil }
func (t trimDriver) Applied() (float64, error) { return t.p.Applied(), nil }

// pwmStats: the duty the PWM loop actually produced over its last window, the worst edge lateness (µs) and the
// real-time priority status (shown on /api/trim to verify the pulses are not disturbed by CPU load).
func (t trimDriver) pwmStats() (float64, int64, string) {
	d, l, rt := t.p.Stats()
	return d, l.Microseconds(), rt
}

// pwmHz: the trim PWM rate. 1 kHz cost ~40 % of a CPU core on the control board (2 wake-ups per cycle, each
// with system calls) and starved s19volt's own web server; 100 Hz is 10x lighter and the 470 uF capacitor
// (~2.2 s time constant) still smooths it (ripple ~0.5 %).
const pwmHz = 100

var drv *trimDriver // nil = no trim hardware (board layout not the tested one): stock voltage

// setupTrimDriver starts the PWM at 0 % (pin LOW = stock voltage). It refuses on anything but the
// tested control-board layout (package ch3 checks address, width, boot default and plug-detect).
func setupTrimDriver() {
	p, pin, err := ch3.Open(pwmHz)
	if err != nil {
		fmt.Println("trim: refused:", err, "- no trim (stock voltage)")
		return
	}
	drv = &trimDriver{p}
	fmt.Printf("trim: gpio%d, PWM %d Hz, starts at 0 %%\n", pin.GPIO, pwmHz)
}

// shutdownGlide caps the exit glide: a stop/kill must not hang (init gives a service ~1-2 min).
const shutdownGlide = 60 * time.Second

// closeTrimDriver on exit: glide toward 0 % at the setup pace (5 % per 2 s) for at most shutdownGlide, then
// force the pin LOW (stock voltage) whatever is left.
func closeTrimDriver() {
	if drv == nil {
		return
	}
	t0 := time.Now()
	if err := glide(0, plainSleep, func() bool { return time.Since(t0) >= shutdownGlide }); err != nil {
		fmt.Println("trim: exit glide:", err, "- forcing the pin LOW")
	}
	drv.p.Close()
}
