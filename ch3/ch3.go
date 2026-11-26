// Package ch3 drives the PSU-trim optocoupler from the S19 control board's EMPTY 4th hashboard
// connector (V2 of the trim mod, 2026-11-24): its reset line is an FPGA AXI GPIO output that nothing
// uses when no board is plugged in.
//
// Safety rules, all enforced here:
//   - the pin is found BY ADDRESS (Linux GPIO numbers shift between kernels), and only on the tested
//     layout: reset block /amba_pl/gpio@41210000, 5 bits, boot default 0x10 (all resets LOW at boot);
//   - refuse if plug-detect says a board is in the 4th connector (the pin is then a real reset line);
//   - writes go through sysfs only (the kernel keeps a shadow register and serialises writes; the register
//     also holds the boards' reset lines and the kernel's heartbeat LED: direct /dev/mem writes were tried on
//     2026-11-25 and froze bosminer's chain monitor - never write it directly);
//   - LOW = LED dark = stock voltage: Open starts low, Close ends low, glides never jump.
package ch3

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	resetLabel = "/amba_pl/gpio@41210000"
	plugLabel  = "/amba_pl/gpio@41200000"
	dtDir      = "/proc/device-tree/amba_pl/gpio@41210000/"
	bit        = 3 // 4th connector
)

// Pin describes the detected 4th-connector reset line.
type Pin struct {
	GPIO int    // Linux GPIO number (e.g. 900)
	Plug string // sysfs value file of the matching plug-detect input
}

func chipBase(label string, n int) (int, bool) {
	cs, _ := filepath.Glob("/sys/class/gpio/gpiochip*")
	for _, c := range cs {
		l, _ := os.ReadFile(c + "/label")
		ng, _ := os.ReadFile(c + "/ngpio")
		b, _ := os.ReadFile(c + "/base")
		if strings.TrimSpace(string(l)) == label && strings.TrimSpace(string(ng)) == strconv.Itoa(n) {
			v, err := strconv.Atoi(strings.TrimSpace(string(b)))
			return v, err == nil
		}
	}
	return 0, false
}

func dtWord(name string) (uint32, bool) {
	b, err := os.ReadFile(dtDir + name)
	if err != nil || len(b) != 4 {
		return 0, false
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), true
}

// Detect returns the pin only if the control board matches the tested layout exactly.
func Detect() (Pin, error) {
	if w, ok := dtWord("xlnx,gpio-width"); !ok || w != 5 {
		return Pin{}, errors.New("reset GPIO block missing or not 5 bits wide")
	}
	if d, ok := dtWord("xlnx,dout-default"); !ok || d != 0x10 {
		return Pin{}, errors.New("reset GPIO boot default is not 0x10 (pin would not boot low)")
	}
	rb, ok1 := chipBase(resetLabel, 5)
	pb, ok2 := chipBase(plugLabel, 4)
	if !ok1 || !ok2 {
		return Pin{}, errors.New("GPIO chips not found by address")
	}
	p := Pin{GPIO: rb + bit, Plug: fmt.Sprintf("/sys/class/gpio/gpio%d/value", pb+bit)}
	if b, err := os.ReadFile(p.Plug); err == nil && len(b) > 0 && b[0] == '1' {
		return Pin{}, errors.New("a hashboard is plugged into the 4th connector")
	}
	return p, nil
}

func (p Pin) dir() string { return fmt.Sprintf("/sys/class/gpio/gpio%d", p.GPIO) }

// Output exports the pin as an output starting LOW and returns its value file.
func (p Pin) Output() (*os.File, error) {
	if _, err := os.Stat(p.dir()); err != nil {
		if err := os.WriteFile("/sys/class/gpio/export", []byte(strconv.Itoa(p.GPIO)), 0); err != nil {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := os.WriteFile(p.dir()+"/direction", []byte("low"), 0); err != nil { // "low" = output at 0
		return nil, err
	}
	return os.OpenFile(p.dir()+"/value", os.O_WRONLY, 0)
}

// PWM drives the pin with software PWM; the board-side capacitor turns it into a steady LED current.
// Target changes glide at GlideRate %/s (same pace as the ESP32 firmware: 0.5 % per 50 ms).
type PWM struct {
	Hz        int
	GlideRate float64

	mu      sync.Mutex
	target  float64
	applied float64
	f       *os.File
	stop    chan struct{}
	done    chan struct{}

	// self-measurement over the last statWindow: the duty the pin actually had (time high / time) and the
	// worst lateness of an edge vs its schedule. Reported by Stats().
	stHigh, stTotal time.Duration
	stLate          time.Duration
	stAt            time.Time
	lastDuty        float64
	lastLate        time.Duration
	rt              string // real-time priority status ("SCHED_FIFO 20" or the error)
}

const (
	rtPriority = 20               // moderate SCHED_FIFO priority: above normal tasks, far below kernel threads
	statWindow = 10 * time.Second // self-measurement window
)

// Open detects the pin, sets it LOW and starts the PWM loop at 0 %.
func Open(hz int) (*PWM, Pin, error) {
	p, err := Detect()
	if err != nil {
		return nil, p, err
	}
	f, err := p.Output()
	if err != nil {
		return nil, p, err
	}
	d := &PWM{Hz: hz, GlideRate: 10, f: f, stop: make(chan struct{}), done: make(chan struct{})}
	go d.loop()
	return d, p, nil
}

func (d *PWM) write(on bool) {
	if on {
		d.f.WriteAt([]byte("1"), 0)
	} else {
		d.f.WriteAt([]byte("0"), 0)
	}
}

// Set changes the target drive (0..100 %); the applied value glides toward it.
func (d *PWM) Set(pct float64) {
	d.mu.Lock()
	d.target = math.Max(0, math.Min(100, pct))
	d.mu.Unlock()
}

// SetNow sets the drive at once, without the glide: used right before bosminer stops or restarts (the
// boards are about to lose power).
func (d *PWM) SetNow(pct float64) {
	d.mu.Lock()
	d.target = math.Max(0, math.Min(100, pct))
	d.applied = d.target
	d.mu.Unlock()
}

// Target and Applied report the requested and current drive in %.
func (d *PWM) Target() float64  { d.mu.Lock(); defer d.mu.Unlock(); return d.target }
func (d *PWM) Applied() float64 { d.mu.Lock(); defer d.mu.Unlock(); return d.applied }

func (d *PWM) loop() {
	defer close(d.done)
	// The PWM edges must not be delayed by the rest of the process or the system (bosminer, metrics parsing,
	// the fan tool): pin this goroutine to its own OS thread and give that thread real-time priority. The loop
	// sleeps between edges (never spins), so it cannot starve other tasks.
	runtime.LockOSThread()
	d.mu.Lock()
	d.rt = setRealtime(rtPriority)
	d.mu.Unlock()

	period := time.Second / time.Duration(d.Hz)
	step := d.GlideRate * period.Seconds()
	next := time.Now()
	d.stAt = next
	for {
		select {
		case <-d.stop:
			d.write(false)
			return
		default:
		}
		d.mu.Lock()
		if diff := d.target - d.applied; math.Abs(diff) <= step {
			d.applied = d.target
		} else {
			d.applied += math.Copysign(step, diff)
		}
		a := d.applied
		d.mu.Unlock()
		on := time.Duration(float64(period) * a / 100)
		var high time.Duration
		if on > 0 {
			t0 := time.Now()
			d.late(t0.Sub(next))
			d.write(true)
			next = next.Add(on)
			time.Sleep(time.Until(next))
			high = time.Since(t0)
		}
		if on < period {
			t1 := time.Now()
			d.late(t1.Sub(next))
			d.write(false)
			next = next.Add(period - on)
			time.Sleep(time.Until(next))
		}
		d.account(high, period)
		// Fell behind by more than one cycle (system stall): resynchronise. Racing through the missed cycles
		// back to back would put wrong-duty cycles on the pin.
		if time.Until(next) < -period {
			next = time.Now()
		}
	}
}

// late records an edge's lateness vs its schedule.
func (d *PWM) late(l time.Duration) {
	if l > d.stLate {
		d.stLate = l
	}
}

// account adds one cycle to the self-measurement and publishes it every statWindow.
func (d *PWM) account(high, period time.Duration) {
	d.stHigh += high
	d.stTotal += period
	if time.Since(d.stAt) < statWindow || d.stTotal <= 0 {
		return
	}
	d.mu.Lock()
	d.lastDuty = 100 * float64(d.stHigh) / float64(d.stTotal)
	d.lastLate = d.stLate
	d.mu.Unlock()
	d.stHigh, d.stTotal, d.stLate, d.stAt = 0, 0, 0, time.Now()
}

// Stats: the duty actually produced over the last window (%), the worst edge lateness in it, and the
// real-time priority status.
func (d *PWM) Stats() (duty float64, worstLate time.Duration, rt string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastDuty, d.lastLate, d.rt
}

// setRealtime gives the calling OS thread SCHED_FIFO priority prio; returns a status string.
func setRealtime(prio int) string {
	const schedFIFO = 1
	param := struct{ priority int32 }{int32(prio)}
	tid := syscall.Gettid()
	if _, _, e := syscall.RawSyscall(syscall.SYS_SCHED_SETSCHEDULER, uintptr(tid), schedFIFO, uintptr(unsafe.Pointer(&param))); e != 0 {
		return "normal priority (SCHED_FIFO refused: " + e.Error() + ")"
	}
	return fmt.Sprintf("SCHED_FIFO %d", prio)
}

// Close stops the PWM and leaves the pin LOW (stock voltage).
func (d *PWM) Close() {
	close(d.stop)
	<-d.done
	d.f.Close()
}
