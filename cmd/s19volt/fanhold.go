package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Fan hold during a soft reset. bosminer pauses the fans (0 %) while paused and writes 100 % to the fan PWM register
// when it exits, so the fans ran at full speed for the whole 20 s off time of every soft reset (register log,
// 2026-11-25 18:49). While bosminer is down nothing else drives the fans; s19volt writes the register back to the
// pre-reset value until bosminer runs again. The boards have no power during that time, so there is nothing to cool.

const (
	fanPWMBase = 0x42800000 // control-board fan controller (same block s19regs reads "PWM now" from)
	fanPWMOff  = 0x10       // fan PWM channel 1, in percent
	fanPWMOff2 = 0x14       // channel 2: C52 boards drive the fans on two PWMs (bosminer: "C52 (2 PWM's)")
)

// fanPWM reads the fan PWM register (percent); -1 on error.
func fanPWM() int {
	f, err := os.Open("/dev/mem")
	if err != nil {
		return -1
	}
	defer f.Close()
	b, err := syscall.Mmap(int(f.Fd()), fanPWMBase, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return -1
	}
	defer syscall.Munmap(b)
	return int(binary.LittleEndian.Uint32(b[fanPWMOff:]))
}

// fanGuard runs for the life of s19volt: whenever bosminer is not running, or started less than fanProbeHold
// ago (whoever restarted it: a soft reset, a mode switch, a power change, a crash), both fan PWM channels are
// written every millisecond with the last fan speed seen while bosminer ran. bosminer writes 100 % when it exits
// and its start-up probe drives the fans at full speed; this keeps them where they were. Deliberately simple.
func fanGuard() {
	f, err := os.OpenFile("/dev/mem", os.O_RDWR|syscall.O_SYNC, 0)
	if err != nil {
		note("fan guard: %v", err)
		return
	}
	b, err := syscall.Mmap(int(f.Fd()), fanPWMBase, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	f.Close()
	if err != nil {
		note("fan guard: %v", err)
		return
	}
	pid := func() string {
		out, _ := exec.Command("pidof", "bosminer").Output()
		return strings.TrimSpace(string(out))
	}
	last, started := pid(), time.Time{}
	hold := -1 // the fan speed to hold (last seen while bosminer ran steadily)
	check := time.Now()
	for {
		if time.Since(check) >= 200*time.Millisecond {
			check = time.Now()
			p := pid()
			if p != "" && p != last {
				started = time.Now() // a new bosminer (restart)
			}
			last = p
		}
		guarding := last == "" || (!started.IsZero() && time.Since(started) < fanProbeHold)
		if !guarding {
			if v := int(binary.LittleEndian.Uint32(b[fanPWMOff:])); v >= minFan && v < 100 {
				hold = v // steady running: remember the speed (bosminer's fan rule / ours set it)
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if hold >= minFan {
			for _, off := range []int{fanPWMOff, fanPWMOff2} {
				binary.LittleEndian.PutUint32(b[off:], uint32(hold))
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// fanProbeHold: how long after bosminer starts the fan PWM keeps being written (its start-up fan probe).
const fanProbeHold = 15 * time.Second
