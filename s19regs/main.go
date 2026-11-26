// s19regs — proof-of-concept register reader for the S19 hashchain FPGA blocks under
// Braiins OS+ (Zynq "am2" control board). Built 2026-11-17, see ../INVESTIGATION.md.
//
// Layout (cross-checked against DCENT_OS, GPL-3, which recovered it from bosminer.bin and
// read it live on a mining S19): one chain = 16 KB at a physical base 0x43C?0000, four
// 4 KB blocks — common +0x0000, command +0x1000, work-rx +0x2000, work-tx +0x3000.
// Braiins' bitstream identifies itself with VERSION (common+0x00) = 0x00901002, then the
// s9io register map applies. Bitmain's stock am2 bitstream has CTRL at +0x00 instead;
// for anything but 0x00901002 only the raw +0x00/+0x04 words are shown.
//
// SAFETY — bosminer owns these blocks while it mines:
//   - FIFO registers are NEVER touched by `list`/`regs` (reads pop data: would steal
//     bosminer's chip replies or nonces). Only whitelisted status/control offsets, through
//     a read-only mapping.
//   - Only physical bases the kernel lists (UIO maps or /proc/iomem) are mapped: a Zynq
//     AXI read where no slave answers can hang the bus.
//   - `chipread` (one BM1398 register read: frame 0x42) is locked behind --unlock and
//     refuses while bosminer runs unless --race. Use with care.
//
// usage:
//
//	s19regs list                      chain bases found, UIO devices, holders
//	s19regs regs                      decode whitelisted registers of every chain
//	s19regs chipread --base 0x43C00000 --chip N [--reg 0x08] --unlock [--race]
//	s19regs decode-pll 0x…            offline BM1398 PLL0 decode
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	braiinsVersion = 0x00901002
	chainSpan      = 0x4000
	blkCommon      = 0x0000
	blkCmd         = 0x1000
	blkWorkRx      = 0x2000
	blkWorkTx      = 0x3000
	addrInterval   = 3 // S19 NBS1902: 76 chips, addresses 0,3,…,225 (DCENT_OS, stock topol.conf)
)

type field struct {
	name   string
	hi, lo uint
}

type reg struct {
	off    uint32 // offset inside the chain span
	name   string
	fields []field
}

// The ONLY offsets ever read by `regs`. FIFO offsets (cmd +0x00/+0x04, work-rx +0x00,
// work-tx +0x04) are absent on purpose.
var whitelist = []reg{
	{blkCommon + 0x00, "VERSION", []field{{"MINER_TYPE", 31, 28}, {"MODEL", 27, 20}, {"MAJOR", 15, 12}, {"MINOR", 11, 8}, {"PATCH", 7, 0}}},
	{blkCommon + 0x04, "BUILD_ID", nil},
	{blkCommon + 0x08, "CTRL_REG", []field{{"BM139X", 4, 4}, {"ENABLE", 3, 3}, {"MIDSTATE_LOG2", 2, 1}, {"ERR_CNT_CLEAR", 0, 0}}},
	{blkCommon + 0x0c, "STAT_REG", nil},
	{blkCommon + 0x10, "BAUD_REG", nil},
	{blkCommon + 0x14, "WORK_TIME", nil},
	{blkCommon + 0x18, "ERR_COUNTER", nil},
	{blkCmd + 0x08, "CMD_CTRL_REG", []field{{"IRQ_EN", 2, 2}, {"RST_TX_FIFO", 1, 1}, {"RST_RX_FIFO", 0, 0}}},
	{blkCmd + 0x0c, "CMD_STAT_REG", []field{{"IRQ_PEND", 4, 4}, {"TX_FULL", 3, 3}, {"TX_EMPTY", 2, 2}, {"RX_FULL", 1, 1}, {"RX_EMPTY", 0, 0}}},
	{blkWorkRx + 0x08, "WORK_RX_CTRL", nil},
	{blkWorkRx + 0x0c, "WORK_RX_STAT", []field{{"IRQ_PEND", 4, 4}, {"RX_FULL", 1, 1}, {"RX_EMPTY", 0, 0}}},
	{blkWorkTx + 0x08, "WORK_TX_CTRL", nil},
	{blkWorkTx + 0x0c, "WORK_TX_STAT", []field{{"BELOW_THR", 4, 4}, {"TX_FULL", 3, 3}, {"TX_EMPTY", 2, 2}}},
	{blkWorkTx + 0x10, "WORK_TX_IRQ_THR", nil},
	{blkWorkTx + 0x14, "WORK_TX_LAST_ID", nil},
}

func readTrim(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }

type uioDev struct {
	dev, name  string
	addr, size uint64
}

func uios() []uioDev {
	var out []uioDev
	ms, _ := filepath.Glob("/sys/class/uio/uio*")
	for _, m := range ms {
		u := uioDev{dev: "/dev/" + filepath.Base(m), name: readTrim(m + "/name")}
		u.addr, _ = strconv.ParseUint(readTrim(m+"/maps/map0/addr"), 0, 64)
		u.size, _ = strconv.ParseUint(readTrim(m+"/maps/map0/size"), 0, 64)
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].addr < out[j].addr })
	return out
}

// chainBases: 16 KB-aligned bases in the 0x43C0_0000 FPGA window that the kernel itself
// lists (UIO maps or /proc/iomem). Nothing else is ever mapped.
func chainBases() map[uint64]string {
	bases := map[uint64]string{}
	add := func(a uint64, src string) {
		if a >= 0x43C00000 && a < 0x43D00000 {
			b := a &^ 0xFFFF
			if _, ok := bases[b]; !ok {
				bases[b] = src
			}
		}
	}
	for _, u := range uios() {
		add(u.addr, "uio:"+u.name)
	}
	if f, err := os.Open("/proc/iomem"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if i := strings.IndexByte(line, '-'); i > 0 {
				if a, err := strconv.ParseUint(line[:i], 16, 64); err == nil {
					add(a, "iomem:"+strings.TrimSpace(line[strings.IndexByte(line, ':')+1:]))
				}
			}
		}
		f.Close()
	}
	return bases
}

func holders() map[string][]string {
	h := map[string][]string{}
	ps, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range ps {
		comm := readTrim(p + "/comm")
		fds, _ := filepath.Glob(p + "/fd/*")
		for _, fd := range fds {
			if t, err := os.Readlink(fd); err == nil && (strings.HasPrefix(t, "/dev/uio") || t == "/dev/mem") {
				h[t] = append(h[t], fmt.Sprintf("%s(%s)", comm, filepath.Base(p)))
			}
		}
	}
	return h
}

func bosminerRunning() bool {
	ps, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, p := range ps {
		if readTrim(p) == "bosminer" {
			return true
		}
	}
	return false
}

// mapChain maps one chain's 16 KB through /dev/mem (read-only unless write).
func mapChain(base uint64, write bool) ([]byte, func(), error) {
	flags, prot := os.O_RDONLY, syscall.PROT_READ
	if write {
		flags, prot = os.O_RDWR, syscall.PROT_READ|syscall.PROT_WRITE
	}
	f, err := os.OpenFile("/dev/mem", flags|syscall.O_SYNC, 0)
	if err != nil {
		return nil, nil, err
	}
	b, err := syscall.Mmap(int(f.Fd()), int64(base), chainSpan, prot, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return b, func() { syscall.Munmap(b); f.Close() }, nil
}

// MMIO access must be atomic: Go has no volatile, and plain stores to the same address
// with no load between are merged by the compiler — writeReg's header word was silently
// deleted that way (2026-11-17: every write reached the FPGA as a lone value word).
func rd32(b []byte, off uint32) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&b[off])))
}
func wr32(b []byte, off uint32, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[off])), v)
}
func bits(v uint32, hi, lo uint) uint32 { return (v >> lo) & ((1 << (hi - lo + 1)) - 1) }

func cmdList() {
	fmt.Println("chain bases the kernel lists in the FPGA window:")
	bs := chainBases()
	keys := make([]uint64, 0, len(bs))
	for k := range bs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		fmt.Printf("   0x%08x  (%s)\n", k, bs[k])
	}
	h := holders()
	fmt.Println("UIO devices:")
	for _, u := range uios() {
		fmt.Printf("   %-8s %-24s 0x%08x size 0x%-6x held by %s\n", filepath.Base(u.dev), u.name, u.addr, u.size, strings.Join(h[u.dev], ","))
	}
	fmt.Println("/dev/mem held by:", strings.Join(h["/dev/mem"], ","))
}

func cmdRegs() {
	for _, base := range sortedBases() {
		b, closeFn, err := mapChain(base, false)
		if err != nil {
			fmt.Printf("0x%08x: map failed: %v\n", base, err)
			continue
		}
		v0, v4 := rd32(b, 0), rd32(b, 4)
		fmt.Printf("== chain @0x%08x  +0x00=0x%08x +0x04=0x%08x", base, v0, v4)
		if v0 != braiinsVersion {
			fmt.Println("  — not Braiins' bm13xx IP (stock am2 layout or other): raw words only")
			closeFn()
			continue
		}
		fmt.Println("  — Braiins bm13xx IP")
		for _, r := range whitelist {
			v := rd32(b, r.off)
			var fs []string
			for _, f := range r.fields {
				fs = append(fs, fmt.Sprintf("%s=%d", f.name, bits(v, f.hi, f.lo)))
			}
			fmt.Printf("   +0x%04x %-16s 0x%08x  %s\n", r.off, r.name, v, strings.Join(fs, " "))
		}
		closeFn()
	}
}

func sortedBases() []uint64 {
	bs := chainBases()
	keys := make([]uint64, 0, len(bs))
	for k := range bs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// BM1398 PLL0 parameter (reg 0x08), per DCENT_OS pll_model (vendor four-divider):
// bit30 PLLEN, bits 27–16 FBDIV (12-bit), 13–8 REFDIV, 6–4 POSTDIV1, 2–0 POSTDIV2 — the
// post dividers are stored RAW (no −1, unlike BM1362/1370). f = 25·FB/(REF·PD1·PD2).
func decodePLL(v uint32) string {
	fb, ref := bits(v, 27, 16), bits(v, 13, 8)
	pd1, pd2 := bits(v, 6, 4), bits(v, 2, 0)
	if ref == 0 || pd1 == 0 || pd2 == 0 {
		return fmt.Sprintf("0x%08x: a divider is 0 — not a BM1398 PLL0 value", v)
	}
	vco := 25.0 * float64(fb) / float64(ref)
	return fmt.Sprintf("locked=%d en=%d fb=%d ref=%d pd1=%d pd2=%d → VCO %.1f MHz, hash clock %.2f MHz",
		bits(v, 31, 31), bits(v, 30, 30), fb, ref, pd1, pd2, vco, vco/float64(pd1*pd2))
}

func cmdChipRead(args []string) {
	fs := flag.NewFlagSet("chipread", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base, e.g. 0x43C00000 (from `list`)")
	chip := fs.Int("chip", -1, "chip index 0–75 (address = index × 3)")
	regn := fs.String("reg", "0x08", "chip register (0x08 = PLL0 parameter)")
	unlock := fs.Bool("unlock", false, "required: acknowledges this uses the command FIFOs")
	race := fs.Bool("race", false, "allow while bosminer runs (may take one of bosminer's replies)")
	fs.Parse(args)
	if !*unlock {
		fmt.Fprintln(os.Stderr, "chipread is locked: pass --unlock")
		os.Exit(2)
	}
	if bosminerRunning() && !*race {
		fmt.Fprintln(os.Stderr, "bosminer is running: its reader would race us for the reply. Stop it, or pass --race")
		os.Exit(2)
	}
	base, err1 := strconv.ParseUint(*baseS, 0, 64)
	r, err2 := strconv.ParseUint(*regn, 0, 8)
	if err1 != nil || err2 != nil || *chip < 0 || *chip > 75 {
		fmt.Fprintln(os.Stderr, "need --base 0x43C?0000 --chip 0..75 [--reg 0x08]")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		fmt.Fprintf(os.Stderr, "0x%x is not a chain base the kernel lists — refusing (bus hang risk)\n", base)
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "map:", err)
		os.Exit(1)
	}
	defer closeFn()
	if v := rd32(b, 0); v != braiinsVersion {
		fmt.Fprintf(os.Stderr, "VERSION 0x%08x is not Braiins' bm13xx IP — refusing\n", v)
		os.Exit(1)
	}
	addr := byte(*chip * addrInterval)
	val, resp, err := readReg(b, addr, byte(r))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("chip %d (addr 0x%02x) reg 0x%02x: raw % x | value 0x%08x | reply chip 0x%02x reg 0x%02x\n",
		*chip, addr, r, resp[:7], val, resp[4], resp[5])
	if r == 0x08 {
		fmt.Println("PLL0:", decodePLL(val))
	}
}

const (
	cmdRxFifo = blkCmd + 0x00
	cmdTxFifo = blkCmd + 0x04
	cmdStat   = blkCmd + 0x0c
)

// readReg: one BM139x register read (0x42, single chip) and its 2-word reply.
func readReg(b []byte, addr, regn byte) (uint32, []byte, error) {
	if st := rd32(b, cmdStat); bits(st, 0, 0) == 0 {
		return 0, nil, fmt.Errorf("command RX FIFO not empty (stat 0x%x): another reply is pending — refusing", st)
	}
	// read register, single chip: 0x42, len 5 incl. CRC5 (the FPGA appends it)
	wr32(b, cmdTxFifo, binary.LittleEndian.Uint32([]byte{0x42, 0x05, addr, regn}))
	deadline := time.Now().Add(200 * time.Millisecond)
	var words []uint32
	for len(words) < 2 && time.Now().Before(deadline) {
		if bits(rd32(b, cmdStat), 0, 0) == 0 {
			words = append(words, rd32(b, cmdRxFifo))
			continue
		}
		time.Sleep(time.Millisecond)
	}
	if len(words) < 2 {
		return 0, nil, fmt.Errorf("no reply (%d words) — chain unpowered, wrong base/address, or bosminer took it", len(words))
	}
	resp := make([]byte, 8)
	binary.LittleEndian.PutUint32(resp[0:], words[0])
	binary.LittleEndian.PutUint32(resp[4:], words[1])
	if resp[4] != addr || resp[5] != regn {
		return 0, resp, fmt.Errorf("reply is for chip 0x%02x reg 0x%02x, not ours (0x%02x/0x%02x) — probably bosminer's", resp[4], resp[5], addr, regn)
	}
	return binary.BigEndian.Uint32(resp[0:4]), resp, nil
}

// writeReg: one BM139x register write (0x41, single chip), both words back to back.
func writeReg(b []byte, addr, regn byte, v uint32) {
	f := []byte{0x41, 0x09, addr, regn, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	w0, w1 := binary.LittleEndian.Uint32(f[0:4]), binary.LittleEndian.Uint32(f[4:8])
	for bits(rd32(b, cmdStat), 3, 3) == 1 { // TX_FULL
		time.Sleep(100 * time.Microsecond)
	}
	wr32(b, cmdTxFifo, w0)
	wr32(b, cmdTxFifo, w1)
}

// chipwrite: move ONE chip's PLL0 feedback divider by exactly ±1 step (the smallest
// frequency change the PLL can make), keeping REF/PD — then read it back.
func cmdChipWrite(args []string) {
	fs := flag.NewFlagSet("chipwrite", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base, e.g. 0x43C00000")
	chip := fs.Int("chip", -1, "chip index 0–75 (address = index × 3)")
	step := fs.Int("step", 0, "FBDIV step: -1 (slower) or +1 (faster); nothing else is allowed")
	same := fs.Bool("same", false, "diagnostic: rewrite the CURRENT value (no frequency change)")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	if *same {
		*step = 0
	}
	if !*unlock || (!*same && *step != -1 && *step != 1) {
		fmt.Fprintln(os.Stderr, "chipwrite --base 0x43C?0000 --chip N --step -1|+1 --unlock")
		os.Exit(2)
	}
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if err != nil || *chip < 0 || *chip > 75 {
		fmt.Fprintln(os.Stderr, "need --base and --chip 0..75")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		fmt.Fprintf(os.Stderr, "0x%x is not a chain base the kernel lists — refusing\n", base)
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "map:", err)
		os.Exit(1)
	}
	defer closeFn()
	if v := rd32(b, 0); v != braiinsVersion {
		fmt.Fprintf(os.Stderr, "VERSION 0x%08x is not Braiins' bm13xx IP — refusing\n", v)
		os.Exit(1)
	}
	addr := byte(*chip * addrInterval)
	cur, _, err := readReg(b, addr, 0x08)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read PLL0:", err)
		os.Exit(1)
	}
	div, _, err := readReg(b, addr, 0x70)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read 0x70:", err)
		os.Exit(1)
	}
	if bits(cur, 30, 30) != 1 || bits(cur, 31, 31) != 1 {
		fmt.Fprintf(os.Stderr, "PLL0 0x%08x not enabled+locked — refusing\n", cur)
		os.Exit(1)
	}
	fb, ref := int(bits(cur, 27, 16)), int(bits(cur, 13, 8))
	nfb := fb + *step
	vco := 25.0 * float64(nfb) / float64(ref)
	if nfb < 16 || nfb > 250 || vco < 2000 || vco > 3200 {
		fmt.Fprintf(os.Stderr, "new FBDIV %d (VCO %.1f MHz) outside the vendor envelope — refusing\n", nfb, vco)
		os.Exit(1)
	}
	nv := (cur &^ (1 << 31)) // LOCKED is written 0
	nv = (nv &^ (0xFFF << 16)) | uint32(nfb)<<16
	fmt.Printf("chip %d (addr 0x%02x) reg 0x70 = 0x%08x (left untouched)\n", *chip, addr, div)
	fmt.Printf("before: 0x%08x  %s\n", cur, decodePLL(cur))
	fmt.Printf("write : 0x%08x  %s\n", nv, decodePLL(nv))
	writeReg(b, addr, 0x08, nv)
	time.Sleep(10 * time.Millisecond)
	writeReg(b, addr, 0x08, nv)
	// poll the readback every 50 ms for 3 s: when does the chip answer, with what value?
	t0 := time.Now()
	last := ""
	for time.Since(t0) < 3*time.Second {
		time.Sleep(50 * time.Millisecond)
		got, _, err := readReg(b, addr, 0x08)
		line := ""
		if err != nil {
			line = "no reply"
		} else {
			line = fmt.Sprintf("0x%08x %s", got, decodePLL(got))
		}
		if line != last {
			fmt.Printf("+%4d ms: %s\n", time.Since(t0).Milliseconds(), line)
			last = line
		}
	}
}

// rewrite: diagnostic — read register R of one chip and write the SAME value back,
// then poll for replies (does a write transaction alone upset the chain?).
func cmdRewrite(args []string) {
	fs := flag.NewFlagSet("rewrite", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	regn := fs.String("reg", "0x14", "register to rewrite with its own value")
	pad := fs.Int("pad", 0, "experiment: extra zero words after the 2-word frame")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, err1 := strconv.ParseUint(*baseS, 0, 64)
	r, err2 := strconv.ParseUint(*regn, 0, 8)
	if !*unlock || err1 != nil || err2 != nil || *chip < 0 || *chip > 75 {
		fmt.Fprintln(os.Stderr, "rewrite --base 0x43C?0000 --chip N --reg 0xRR --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	cur, _, err := readReg(b, addr, byte(r))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	fmt.Printf("chip %d reg 0x%02x = 0x%08x → writing the same value\n", *chip, r, cur)
	writeReg(b, addr, byte(r), cur)
	for i := 0; i < *pad; i++ {
		wr32(b, cmdTxFifo, 0)
	}
	t0 := time.Now()
	last := ""
	for time.Since(t0) < 3*time.Second {
		time.Sleep(50 * time.Millisecond)
		v, _, err := readReg(b, addr, byte(r))
		line := "no reply"
		if err == nil {
			line = fmt.Sprintf("0x%08x", v)
		}
		if line != last {
			fmt.Printf("+%4d ms: %s\n", time.Since(t0).Milliseconds(), line)
			last = line
		}
	}
}

// ---------------- live fan target (bosminer's own manual-mode setpoint, in its memory) ----------------
// Found 2026-11-17: bosminer's fan loop re-reads the manual fan % every tick from a config struct
// on its heap; the one live copy sat at 0xb59c99d4 (stable across restarts so far). Layout around the
// u32 target T: T,0,0x1f,0,1,minfans(4),2000,600,…,f32 target°C,hot? — we match the f32 run
// 40,159,80,110,45 (target, dangerous, hot, …) located 48 bytes after T, plus T==this config's value
// range. Changing T makes bosminer's own loop drive the PWM (safety overrides untouched).
// the hot °C (3rd float) is user-tunable, so it is matched as a range, not a value
func f32s(fs ...float32) []byte {
	b := make([]byte, 4*len(fs))
	for i, f := range fs {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// The fan settings struct is found by the miner's OWN temperatures (from /etc/bosminer.toml [temp_control]),
// stored as float32: target_temp and dangerous_temp side by side, hot_temp right after. Its address changes on
// every bosminer start (heap), so it is searched, then cached per bosminer pid. A candidate is only trusted when
// bosminer's fan loop follows a write on the hardware PWM register (fanset) — nothing is assumed from layout.
type tempCfg struct{ target, hot, dangerous float32 }

func readTempCfg() (tempCfg, error) {
	b, err := os.ReadFile("/etc/bosminer.toml")
	if err != nil {
		return tempCfg{}, err
	}
	c, in := tempCfg{}, false
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[") {
			in = l == "[temp_control]"
			continue
		}
		if !in {
			continue
		}
		kv := strings.SplitN(l, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 32)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(kv[0]) {
		case "target_temp":
			c.target = float32(v)
		case "hot_temp":
			c.hot = float32(v)
		case "dangerous_temp":
			c.dangerous = float32(v)
		}
	}
	if c.target == 0 || c.dangerous == 0 {
		return c, fmt.Errorf("/etc/bosminer.toml has no [temp_control] target_temp/dangerous_temp (manual fan mode needs them)")
	}
	return c, nil
}

func fanCache(pid int) string { return fmt.Sprintf("/tmp/s19regs-fan-%d", pid) }

func bosminerPID() (int, error) {
	ps, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, p := range ps {
		if readTrim(p) == "bosminer" {
			return strconv.Atoi(filepath.Base(filepath.Dir(p)))
		}
	}
	return 0, fmt.Errorf("bosminer not running")
}

// fanCandidates: addresses of the fan target whose struct holds this miner's temperatures (cached one first).
func fanCandidates(pid int, useCache bool) ([]uint64, *os.File, error) {
	cfg, err := readTempCfg()
	if err != nil {
		return nil, nil, err
	}
	sig := f32s(cfg.target, cfg.dangerous)
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", pid), os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	check := func(t uint64) bool {
		buf := make([]byte, 0x40)
		if _, err := f.ReadAt(buf, int64(t)); err != nil {
			return false
		}
		hot := math.Float32frombits(binary.LittleEndian.Uint32(buf[0x34:]))
		return binary.LittleEndian.Uint32(buf[0:]) <= 100 && string(buf[0x2c:0x34]) == string(sig) &&
			(cfg.hot == 0 || hot == cfg.hot)
	}
	// bosminer keeps many copies of these settings (about a dozen seen) and recreates the live one on some
	// events (pause/resume, reloads): a cached address can still pass check() yet be dead. Callers retry
	// with useCache=false when the cached one doesn't move the PWM.
	if b, err := os.ReadFile(fanCache(pid)); useCache && err == nil {
		if t, err := strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64); err == nil && check(t) {
			return []uint64{t}, f, nil
		}
	}
	var out []uint64
	mp, _ := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	for _, line := range strings.Split(string(mp), "\n") {
		fs := strings.Fields(line)
		if len(fs) < 2 || fs[1] != "rw-p" || len(fs) >= 6 && strings.HasPrefix(fs[5], "/") {
			continue
		}
		var lo, hi uint64
		fmt.Sscanf(fs[0], "%x-%x", &lo, &hi)
		if hi-lo > 64<<20 {
			continue
		}
		buf := make([]byte, hi-lo)
		if _, err := f.ReadAt(buf, int64(lo)); err != nil {
			continue
		}
		for off := 0; ; {
			i := strings.Index(string(buf[off:]), string(sig))
			if i < 0 {
				break
			}
			if t := lo + uint64(off+i) - 0x2c; off+i >= 0x2c && check(t) {
				out = append(out, t)
			}
			off += i + 1
		}
	}
	return out, f, nil
}

func cmdFan(args []string) {
	pid, err := bosminerPID()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cands, f, err := fanCandidates(pid, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fan:", err)
		os.Exit(1)
	}
	defer f.Close()
	if len(cands) == 0 {
		fmt.Fprintln(os.Stderr, "fan settings not found in bosminer's memory (needs [temp_control] mode = \"manual\") — refusing; bosminer keeps its own fan control")
		os.Exit(1)
	}
	rdT := func(t uint64) uint32 {
		b := make([]byte, 4)
		f.ReadAt(b, int64(t))
		return binary.LittleEndian.Uint32(b)
	}
	pwm := func() uint32 {
		mf, err := os.Open("/dev/mem")
		if err != nil {
			return 0
		}
		defer mf.Close()
		b, err := syscall.Mmap(int(mf.Fd()), 0x42800000, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return 0
		}
		defer syscall.Munmap(b)
		return rd32(b, 0x10)
	}
	if len(args) == 0 { // fanget: the live struct is the one whose value the PWM shows
		for _, t := range cands {
			if rdT(t) == pwm() || len(cands) == 1 {
				_ = os.WriteFile(fanCache(pid), []byte(fmt.Sprintf("%#x", t)), 0o600)
				fmt.Printf("bosminer %d: fan target @%#x = %d %%   (PWM now %d %%)\n", pid, t, rdT(t), pwm())
				return
			}
		}
		fmt.Printf("bosminer %d: live fan target not identified (%d candidates, PWM now %d %%)\n", pid, len(cands), pwm())
		os.Exit(1)
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 || n > 100 {
		fmt.Fprintln(os.Stderr, "fanset N: 0..100 (the guard enforces the measured floor)")
		os.Exit(2)
	}
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(n))
	try := func(cands []uint64) bool {
		// one candidate at a time: keep the write only where bosminer's loop follows it (≤ 3 s);
		// otherwise restore the old value — never leave a write in a struct the loop doesn't read.
		for _, t := range cands {
			old := make([]byte, 4)
			f.ReadAt(old, int64(t))
			if _, err := f.WriteAt(b, int64(t)); err != nil {
				fmt.Fprintln(os.Stderr, "write:", err)
				os.Exit(1)
			}
			// bosminer ramps the fan gradually: the write is confirmed as soon as the PWM reaches n OR starts moving
			// toward it (proof that its fan loop reads this struct); a big jump (25 -> 60 %) takes several seconds.
			p0 := pwm()
			ok := false
			for i := 0; i < 50; i++ {
				p := pwm()
				if p == uint32(n) || (p != p0 && (p > p0) == (uint32(n) > p0)) {
					ok = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if ok {
				_ = os.WriteFile(fanCache(pid), []byte(fmt.Sprintf("%#x", t)), 0o600)
				fmt.Printf("bosminer %d: fan target %d %% @%#x; PWM now %d %%\n", pid, n, t, pwm())
				return true
			}
			f.WriteAt(old, int64(t))
		}
		return false
	}
	if try(cands) {
		return
	}
	tried := len(cands)
	os.Remove(fanCache(pid))
	f.Close()
	if cands, f, err = fanCandidates(pid, false); err == nil {
		defer f.Close()
		if try(cands) {
			return
		}
		tried += len(cands)
	}
	fmt.Fprintf(os.Stderr, "no candidate moved the PWM (%d tried) — all restored\n", tried)
	os.Exit(1)
}

// regdump: read registers 0x00..0xFC (step 4) of one chip — read-only diagnostics.
func cmdRegDump(args []string) {
	fs := flag.NewFlagSet("regdump", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	unlock := fs.Bool("unlock", false, "required (uses the command FIFOs)")
	fs.Parse(args)
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if !*unlock || err != nil || *chip < 0 || *chip > 75 {
		fmt.Fprintln(os.Stderr, "regdump --base 0x43C?0000 --chip N --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	for r := 0; r <= 0xfc; r += 4 {
		var v uint32
		ok := false
		for try := 0; try < 3 && !ok; try++ {
			if x, _, err := readReg(b, addr, byte(r)); err == nil {
				v, ok = x, true
			} else {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if ok {
			fmt.Printf("%02x %08x\n", r, v)
		} else {
			fmt.Printf("%02x --------\n", r)
		}
	}
}

// coreinit: re-send bosminer's per-chip core init writes (reg 0x3C: 0x80008710 then 0x80008050, the BM1398
// values documented by DCENT_OS and present on every chip) to ONE chip — a per-chip soft re-init.
func cmdCoreInit(args []string) {
	fs := flag.NewFlagSet("coreinit", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if !*unlock || err != nil || *chip < 0 || *chip > 75 {
		fmt.Fprintln(os.Stderr, "coreinit --base 0x43C?0000 --chip N --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	before, _, err := readReg(b, addr, 0x3c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read 0x3c:", err)
		os.Exit(1)
	}
	// 0x3C holds the last mailbox command: bosminer's 0x80008050, or one of our core READ commands
	// (bit 15 clear, low byte 0xFF) — anything else means we don't know what we'd be overwriting.
	if before != 0x80008050 && !(before&0x8000 == 0 && before&0xff == 0xff) {
		fmt.Fprintf(os.Stderr, "0x3c is 0x%08x, not bosminer's value or a read command — refusing\n", before)
		os.Exit(1)
	}
	writeReg(b, addr, 0x3c, 0x80008710)
	time.Sleep(10 * time.Millisecond)
	writeReg(b, addr, 0x3c, 0x80008050)
	time.Sleep(20 * time.Millisecond)
	after, _, _ := readReg(b, addr, 0x3c)
	ac, _, _ := readReg(b, addr, 0xac)
	fmt.Printf("chip %d: core init re-sent; 0x3c 0x%08x → 0x%08x, 0xac now 0x%08x\n", *chip, before, after, ac)
}

// regwrite: write VALUE to one register of ONE chip and read it back — only registers on the
// experiment allowlist (status/reset block), never PLL/address/config by accident.
var regwriteAllow = map[byte]bool{0x18: true, 0x48: true, 0x4c: true, 0xa8: true, 0xac: true}

func cmdRegWrite(args []string) {
	fs := flag.NewFlagSet("regwrite", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	regS := fs.String("reg", "", "register (allowlist: 0x18 0x48 0xa8 0xac)")
	valS := fs.String("value", "", "32-bit value")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, e1 := strconv.ParseUint(*baseS, 0, 64)
	r, e2 := strconv.ParseUint(*regS, 0, 8)
	v, e3 := strconv.ParseUint(*valS, 0, 32)
	if !*unlock || e1 != nil || e2 != nil || e3 != nil || *chip < 0 || *chip > 75 || !regwriteAllow[byte(r)] {
		fmt.Fprintln(os.Stderr, "regwrite --base 0x43C?0000 --chip N --reg 0x18|0x48|0xa8|0xac --value 0x… --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	before, _, _ := readReg(b, addr, byte(r))
	writeReg(b, addr, byte(r), uint32(v))
	time.Sleep(20 * time.Millisecond)
	after, _, err := readReg(b, addr, byte(r))
	if err != nil {
		fmt.Printf("chip %d reg 0x%02x: wrote 0x%08x (was 0x%08x); readback failed: %v\n", *chip, r, v, before, err)
		return
	}
	fmt.Printf("chip %d reg 0x%02x: 0x%08x → wrote 0x%08x → reads 0x%08x\n", *chip, r, before, v, after)
}

// pllcycle: stop ONE chip's hash clock (PLL0 enable bit 30 cleared, dividers unchanged), wait, then
// restart it with the same dividers (written twice, as bosminer does) — restarts the whole hashing
// array of that chip without touching the others. Refuses unless the PLL was enabled + locked.
func cmdPllCycle(args []string) {
	fs := flag.NewFlagSet("pllcycle", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	offMs := fs.Int("off-ms", 100, "how long the clock stays off")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if !*unlock || err != nil || *chip < 0 || *chip > 75 || *offMs < 1 || *offMs > 60000 {
		fmt.Fprintln(os.Stderr, "pllcycle --base 0x43C?0000 --chip N [--off-ms 100] --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	cur, _, err := readReg(b, addr, 0x08)
	if err != nil || bits(cur, 30, 30) != 1 || bits(cur, 31, 31) != 1 {
		fmt.Fprintf(os.Stderr, "PLL0 0x%08x not enabled+locked (err %v) — refusing\n", cur, err)
		os.Exit(1)
	}
	on := cur &^ (1 << 31) // LOCKED is written 0
	off := on &^ (1 << 30) // PLLEN cleared
	writeReg(b, addr, 0x08, off)
	time.Sleep(time.Duration(*offMs) * time.Millisecond)
	writeReg(b, addr, 0x08, on)
	time.Sleep(10 * time.Millisecond)
	writeReg(b, addr, 0x08, on)
	t0 := time.Now()
	for time.Since(t0) < 2*time.Second {
		time.Sleep(50 * time.Millisecond)
		if v, _, err := readReg(b, addr, 0x08); err == nil && bits(v, 31, 31) == 1 {
			fmt.Printf("chip %d: clock off %d ms, back on and locked after %d ms: %s\n", *chip, *offMs,
				time.Since(t0).Milliseconds(), decodePLL(v))
			return
		}
	}
	fmt.Printf("chip %d: clock re-enabled but no locked readback within 2 s — check it\n", *chip)
}

// pllSolve: BM1398 PLL0 value for a target MHz (REF 2 then 1, PD1 ≥ PD2 in 1..7, FB 16..250,
// VCO 2000..3200 MHz, refdiv-1 VCO ≤ 3125), nearest achievable frequency.
var pllRefs = []int{2, 1} // reference dividers the solver may use (pllset --ref narrows it)

func pllSolve(target float64) (uint32, float64) {
	best, bestF := uint32(0), -1.0
	for _, ref := range pllRefs {
		for pd1 := 1; pd1 <= 7; pd1++ {
			for pd2 := 1; pd2 <= pd1; pd2++ {
				fb := int(math.Round(target * float64(ref*pd1*pd2) / 25))
				if fb < 16 || fb > 250 {
					continue
				}
				vco := 25 * float64(fb) / float64(ref)
				if vco < 2000 || vco > 3200 || (ref == 1 && vco > 3125) {
					continue
				}
				f := vco / float64(pd1*pd2)
				if bestF < 0 || math.Abs(f-target) < math.Abs(bestF-target) {
					best = 1<<30 | uint32(fb)<<16 | uint32(ref)<<8 | uint32(pd1)<<4 | uint32(pd2)
					bestF = f
				}
			}
		}
	}
	return best, bestF
}

// pllset: ramp ONE chip's clock to --mhz in ≤ 6.25 MHz steps every 100 ms (bosminer's own pace).
func cmdPllSet(args []string) {
	fs := flag.NewFlagSet("pllset", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	mhz := fs.Float64("mhz", 0, "target hash clock MHz (40..600)")
	refOnly := fs.Int("ref", 0, "force reference divider 1 or 2 (0 = solver's choice)")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if !*unlock || err != nil || *chip < 0 || *chip > 75 || *mhz < 40 || *mhz > 600 {
		fmt.Fprintln(os.Stderr, "pllset --base 0x43C?0000 --chip N --mhz 40..600 --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	cur, _, err := readReg(b, addr, 0x08)
	if err != nil || bits(cur, 30, 30) != 1 {
		fmt.Fprintf(os.Stderr, "PLL0 0x%08x not enabled (err %v) — refusing\n", cur, err)
		os.Exit(1)
	}
	if *refOnly == 1 || *refOnly == 2 {
		pllRefs = []int{*refOnly}
	}
	fb, ref := float64(bits(cur, 27, 16)), float64(bits(cur, 13, 8))
	f := 25 * fb / ref / float64(bits(cur, 6, 4)*bits(cur, 2, 0))
	start := f
	if math.Abs(f-*mhz) < 0.01 { // same clock, other dividers (e.g. --ref): one direct switch
		v, _ := pllSolve(*mhz)
		if v != 0 && v != cur&^(1<<31) {
			writeReg(b, addr, 0x08, v)
			time.Sleep(10 * time.Millisecond)
			writeReg(b, addr, 0x08, v)
			time.Sleep(100 * time.Millisecond)
		}
		f = *mhz
	}
	for f != *mhz {
		next := *mhz
		if math.Abs(next-f) > 6.25 {
			next = f + math.Copysign(6.25, *mhz-f)
		}
		v, _ := pllSolve(next)
		if v == 0 {
			fmt.Fprintf(os.Stderr, "no PLL solution for %.2f MHz — stopping at %.2f\n", next, f)
			os.Exit(1)
		}
		writeReg(b, addr, 0x08, v)
		time.Sleep(10 * time.Millisecond)
		writeReg(b, addr, 0x08, v)
		time.Sleep(100 * time.Millisecond)
		f = next
	}
	v, _, err := readReg(b, addr, 0x08)
	if err != nil {
		fmt.Printf("chip %d: ramped %.2f → %.2f MHz; readback failed: %v\n", *chip, start, *mhz, err)
		return
	}
	fmt.Printf("chip %d: ramped %.2f → %s\n", *chip, start, decodePLL(v))
}

// coreread: read ONE core register of ONE chip through the 0x3C mailbox (bits 31 and 15 clear = single
// core, read). The chip answers with a reply frame for register 0x40: [7:0] value, [15:8] FOUND,
// [24:16] core id echo (BM1397 layout, skot/BM1397; go-bm13xx ReadCoreRegister). Fallback: read 0x40.
func cmdCoreRead(args []string) {
	fs := flag.NewFlagSet("coreread", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	core := fs.Int("core", 0, "core id 0..511")
	creg := fs.Int("creg", 4, "core register 0..15")
	n := fs.Int("n", 1, "read cores core..core+n-1")
	waitMs := fs.Int("wait-ms", 60, "how long to wait for each core's reply")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if !*unlock || err != nil || *chip < 0 || *chip > 75 || *core < 0 || *core+*n > 512 || *creg < 0 || *creg > 15 || *n < 1 {
		fmt.Fprintln(os.Stderr, "coreread --base 0x43C?0000 --chip N --core C [--n K] --creg R --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	addr := byte(*chip * addrInterval)
	misses := 0
	for c := *core; c < *core+*n; c++ {
		if st := rd32(b, cmdStat); bits(st, 0, 0) == 0 {
			time.Sleep(20 * time.Millisecond) // someone else's reply pending — let bosminer take it
		}
		cmd := uint32(c)<<16 | uint32(*creg)<<8 | 0xff
		writeReg(b, addr, 0x3c, cmd)
		var got uint32
		ok := false
		deadline := time.Now().Add(time.Duration(*waitMs) * time.Millisecond)
		for time.Now().Before(deadline) && !ok {
			if bits(rd32(b, cmdStat), 0, 0) == 0 {
				w0 := rd32(b, cmdRxFifo)
				for i := 0; i < 20 && bits(rd32(b, cmdStat), 0, 0) == 1; i++ {
					time.Sleep(time.Millisecond)
				}
				w1 := rd32(b, cmdRxFifo)
				resp := make([]byte, 8)
				binary.LittleEndian.PutUint32(resp[0:], w0)
				binary.LittleEndian.PutUint32(resp[4:], w1)
				if resp[4] == addr && resp[5] == 0x40 {
					got, ok = binary.BigEndian.Uint32(resp[0:4]), true
				}
				continue
			}
			time.Sleep(time.Millisecond)
		}
		if !ok { // fallback: explicit read of 0x40
			if v, _, err := readReg(b, addr, 0x40); err == nil && int(v>>16)&0x1ff == c {
				got, ok = v, true
			}
		}
		if ok {
			misses = 0
			fmt.Printf("core %3d reg %d: value 0x%02x found 0x%02x echo %d (raw 0x%08x)\n", c, *creg, got&0xff, (got>>8)&0xff, (got>>16)&0x1ff, got)
		} else {
			misses++
			fmt.Printf("core %3d reg %d: no reply\n", c, *creg)
			if misses >= 5 {
				fmt.Println("5 misses in a row — stopping")
				return
			}
		}
	}
}

// corewrite: write ONE core register (allowlist: 0 clock delay, 5 hash clock ctrl) of one core or of all
// cores of ONE chip via the 0x3C mailbox: (all<<31)|(core<<16)|0x8000|(creg<<8)|value.
func cmdCoreWrite(args []string) {
	fs := flag.NewFlagSet("corewrite", flag.ExitOnError)
	baseS := fs.String("base", "", "chain physical base")
	chip := fs.Int("chip", -1, "chip index 0–75")
	core := fs.Int("core", 0, "core id 0..155 (ignored with --all)")
	all := fs.Bool("all", false, "all cores of the chip")
	creg := fs.Int("creg", -1, "core register (allowlist: 0, 5)")
	val := fs.Int("value", -1, "8-bit value")
	unlock := fs.Bool("unlock", false, "required")
	fs.Parse(args)
	base, err := strconv.ParseUint(*baseS, 0, 64)
	if !*unlock || err != nil || *chip < 0 || *chip > 75 || (*creg != 0 && *creg != 5) || *val < 0 || *val > 255 || *core < 0 || *core > 155 {
		fmt.Fprintln(os.Stderr, "corewrite --base 0x43C?0000 --chip N (--core C | --all) --creg 0|5 --value 0..255 --unlock")
		os.Exit(2)
	}
	if _, ok := chainBases()[base]; !ok {
		os.Exit(2)
	}
	b, closeFn, err := mapChain(base, true)
	if err != nil || rd32(b, 0) != braiinsVersion {
		fmt.Fprintln(os.Stderr, "map/version check failed")
		os.Exit(1)
	}
	defer closeFn()
	cmd := uint32(*core)<<16 | 0x8000 | uint32(*creg)<<8 | uint32(*val)
	if *all {
		cmd = 0x80000000 | 0x8000 | uint32(*creg)<<8 | uint32(*val)
	}
	writeReg(b, byte(*chip*addrInterval), 0x3c, cmd)
	fmt.Printf("chip %d: core write 0x%08x (creg %d = 0x%02x, %s)\n", *chip, cmd, *creg, *val,
		map[bool]string{true: "all cores", false: fmt.Sprintf("core %d", *core)}[*all])
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: s19regs list | regs | chipread --base 0x43C00000 --chip N [--reg 0x08] --unlock [--race] | decode-pll 0x…")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "list":
		cmdList()
	case "regs":
		cmdRegs()
	case "chipread":
		cmdChipRead(os.Args[2:])
	case "chipwrite":
		cmdChipWrite(os.Args[2:])
	case "rewrite":
		cmdRewrite(os.Args[2:])
	case "corewrite":
		cmdCoreWrite(os.Args[2:])
	case "coreread":
		cmdCoreRead(os.Args[2:])
	case "pllset":
		cmdPllSet(os.Args[2:])
	case "pllcycle":
		cmdPllCycle(os.Args[2:])
	case "regwrite":
		cmdRegWrite(os.Args[2:])
	case "coreinit":
		cmdCoreInit(os.Args[2:])
	case "regdump":
		cmdRegDump(os.Args[2:])
	case "fanget":
		cmdFan(nil)
	case "fanset":
		cmdFan(os.Args[2:])
	case "decode-pll":
		v, err := strconv.ParseUint(os.Args[2], 0, 32)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decode-pll 0x…")
			os.Exit(2)
		}
		fmt.Println(decodePLL(uint32(v)))
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}
