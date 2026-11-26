// ch3test — meter tests for the V2 trim drive: the empty 4th connector's reset line (package ch3).
// Detection, sysfs-only writes and "always LOW on exit" all come from package ch3.
//
//	ch3test detect                   print the detected pin, change nothing
//	ch3test toggle [-secs 300]       5 s high / 5 s low (find the pin with a meter)
//	ch3test high   [-secs 120]       hold high (load test with 1 kOhm)
//	ch3test pwm    -duty 50 [-secs 120] [-hz 1000]   software PWM (capacitor check)
//	ch3test low                      force low and quit
//	ch3test hold [-secs 3600] [-glide 1]   PWM at the duty written to /tmp/ch3duty (0..100),
//	                                 gliding -glide %/s; missing file = 0 %. For live trim tests.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"s19psu/ch3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: ch3test detect|toggle|high|pwm|low [flags]")
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	secs := fs.Int("secs", 300, "duration")
	duty := fs.Float64("duty", 50, "PWM duty %")
	hz := fs.Int("hz", 1000, "PWM frequency")
	glide := fs.Float64("glide", 1, "hold mode: glide rate %/s")
	fs.Parse(os.Args[2:])

	if mode == "detect" {
		p, err := ch3.Detect()
		if err != nil {
			fmt.Println("not the tested layout:", err)
			os.Exit(1)
		}
		fmt.Printf("4th-connector reset = gpio%d (plug-detect %s)\n", p.GPIO, p.Plug)
		return
	}
	d, p, err := ch3.Open(*hz) // starts LOW at 0 %
	if err != nil {
		fmt.Fprintln(os.Stderr, "refusing:", err)
		os.Exit(1)
	}
	fmt.Printf("gpio%d\n", p.GPIO)
	defer func() { d.Close(); fmt.Println(time.Now().Format("15:04:05"), "pin LOW (stock), exit") }()
	d.GlideRate = 100000 // meter tests: no glide, instant levels
	if mode == "hold" {
		d.GlideRate = *glide
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	end := time.Now().Add(time.Duration(*secs) * time.Second)
	wait := func(t time.Duration) bool { // false = stop
		select {
		case <-sig:
			return false
		case <-time.After(t):
			return time.Now().Before(end)
		}
	}
	switch mode {
	case "low":
	case "high":
		d.Set(100)
		fmt.Println(time.Now().Format("15:04:05"), "pin HIGH")
		for wait(time.Second) {
		}
	case "toggle":
		for on := true; ; on = !on {
			d.Set(map[bool]float64{true: 100, false: 0}[on])
			fmt.Println(time.Now().Format("15:04:05"), map[bool]string{true: "HIGH (3.3 V)", false: "LOW (0 V)"}[on])
			if !wait(5 * time.Second) {
				break
			}
		}
	case "pwm":
		d.Set(*duty)
		fmt.Printf("%s PWM %.0f%% at %d Hz (expect ~%.2f V on the capacitor without the LED)\n", time.Now().Format("15:04:05"), *duty, *hz, 3.3**duty/100)
		for wait(time.Second) {
		}
	case "hold":
		last := -1.0
		for {
			v := 0.0
			if b, err := os.ReadFile("/tmp/ch3duty"); err == nil {
				fmt.Sscanf(string(b), "%g", &v)
			}
			if v < 0 || v > 100 {
				v = 0
			}
			if v != last {
				d.Set(v)
				fmt.Printf("%s target %.1f%% (gliding %.1f%%/s from %.1f%%)\n", time.Now().Format("15:04:05"), v, *glide, d.Applied())
				last = v
			}
			if !wait(500 * time.Millisecond) {
				break
			}
		}
	default:
		fmt.Println("unknown mode")
	}
}
