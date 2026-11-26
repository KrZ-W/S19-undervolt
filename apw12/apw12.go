// Package apw12: bit-banged I2C to the Bitmain APW12 PSU on the S19 control board, copied from bosminer's own
// traffic (decoded 2026-11-21). Shared by cmd/psugpio (CLI) and cmd/s19volt (service).
package apw12

// Bit-banged I2C to the APW12 PSU on the AXI GPIO block bosminer uses (phys 0x41220000; DATA 0x00, TRI 0x04;
// SDA = bit0, SCL = bit1; 7-bit address 0x10). bosminer leaves both lines as outputs driven high (DATA=3, TRI=0)
// when idle — every transaction here starts only after 50 ms of that idle state, and ends by restoring it.
// Frame: 55 AA LEN CMD payload ckLo ckHi (16-bit LE sum of LEN..payload).

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const base = 0x41220000 // AXI GPIO block bosminer bit-bangs the PSU on

const (
	sda   = 1
	scl   = 2
	addr7 = 0x10
)

type Bus struct {
	m    []byte
	half time.Duration
	// Mark, if set, is called with "LAST_BYTE_NEXT"/"FRAME_SENT" while a frame goes out
	Mark func(string)
}

func MapRW() []byte {
	fd, err := syscall.Open("/dev/mem", syscall.O_RDWR|syscall.O_SYNC, 0)
	if err != nil {
		panic(err)
	}
	m, err := syscall.Mmap(fd, base, 0x1000, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	syscall.Close(fd)
	if err != nil {
		panic(err)
	}
	return m
}

func (b *Bus) data() uint32     { return *(*uint32)(unsafe.Pointer(&b.m[0])) }
func (b *Bus) tri() uint32      { return *(*uint32)(unsafe.Pointer(&b.m[4])) }
func (b *Bus) setData(v uint32) { *(*uint32)(unsafe.Pointer(&b.m[0])) = v }
func (b *Bus) setTri(v uint32)  { *(*uint32)(unsafe.Pointer(&b.m[4])) = v }

func (b *Bus) wait() {
	t := time.Now().Add(b.half)
	for time.Now().Before(t) {
	}
}

// line helpers: SCL is always driven (push-pull, like bosminer); SDA is driven, or released (TRI bit0 = 1) to read.
func (b *Bus) sclHi()  { b.setData(b.data() | scl); b.wait() }
func (b *Bus) sclLo()  { b.setData(b.data() &^ scl); b.wait() }
func (b *Bus) sdaHi()  { b.setTri(b.tri() &^ sda); b.setData(b.data() | sda) }
func (b *Bus) sdaLo()  { b.setTri(b.tri() &^ sda); b.setData(b.data() &^ sda) }
func (b *Bus) sdaRel() { b.setTri(b.tri() | sda) }
func (b *Bus) sdaIn() bool {
	return b.data()&sda != 0
}

func (b *Bus) start() { b.sdaHi(); b.sclHi(); b.sdaLo(); b.wait(); b.sclLo() }
func (b *Bus) stop()  { b.sdaLo(); b.wait(); b.sclHi(); b.sdaHi(); b.wait() }

func (b *Bus) writeByte(v byte) bool { // returns ACK
	for i := 7; i >= 0; i-- {
		if v&(1<<uint(i)) != 0 {
			b.sdaHi()
		} else {
			b.sdaLo()
		}
		b.wait()
		b.sclHi()
		b.sclLo()
	}
	b.sdaRel()
	b.wait()
	b.sclHi()
	ack := !b.sdaIn()
	b.sclLo()
	return ack
}

func (b *Bus) readByte(ack bool) byte {
	b.sdaRel()
	var v byte
	for i := 0; i < 8; i++ {
		b.wait()
		b.sclHi()
		v <<= 1
		if b.sdaIn() {
			v |= 1
		}
		b.sclLo()
	}
	if ack {
		b.sdaLo()
	} else {
		b.sdaHi()
	}
	b.wait()
	b.sclHi()
	b.sclLo()
	return v
}

func (b *Bus) restore() { b.setData(3); b.setTri(0) } // bosminer's idle state

// waitIdle: lines must sit at DATA=3,TRI=0 for `quiet` before we take them.
func (b *Bus) waitIdle(quiet, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	since := time.Now()
	for time.Now().Before(end) {
		if b.data()&3 != 3 || b.tri()&3 != 0 {
			since = time.Now()
		} else if time.Now().Sub(since) >= quiet {
			return true
		}
	}
	return false
}

func PsuFrame(cmd byte, payload ...byte) []byte {
	l := byte(4 + len(payload))
	s := int(l) + int(cmd)
	for _, p := range payload {
		s += int(p)
	}
	f := append([]byte{0x55, 0xAA, l, cmd}, payload...)
	return append(f, byte(s), byte(s>>8))
}

func CheckReply(r []byte, cmd byte) error {
	if len(r) < 6 || r[0] != 0x55 || r[1] != 0xAA {
		return fmt.Errorf("bad preamble")
	}
	l := int(r[2])
	if l < 4 || l+2 > len(r) {
		return fmt.Errorf("bad length %d", l)
	}
	s := 0
	for _, v := range r[2:l] {
		s += int(v)
	}
	if byte(s) != r[l] || byte(s>>8) != r[l+1] {
		return fmt.Errorf("checksum mismatch")
	}
	if r[3] != cmd {
		return fmt.Errorf("reply cmd %02x, want %02x", r[3], cmd)
	}
	return nil
}

// ---- bosminer-exact protocol (decoded from its own traffic, psu-wave-2026-11-21.txt) ----
// SCL period ~3.06 ms (half ~1.53 ms). Each frame byte: START, 0x20 (0x10 W), 0x11, byte, STOP (~84 ms).
// 6.6 ms between transactions. ~357 ms after the frame, each reply byte: START, 0x21 (0x10 R), byte, NACK, STOP.
const (
	bosHalf  = 1530 * time.Microsecond
	bosGap   = 6600 * time.Microsecond
	bosReply = 357 * time.Millisecond
)

func (b *Bus) BosCmd(c byte, payload []byte) ([]byte, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	b.half = bosHalf
	frame := PsuFrame(c, payload...)
	if !b.waitIdle(50*time.Millisecond, 5*time.Second) {
		return nil, fmt.Errorf("bus not idle (DATA=%x TRI=%x)", b.data()&3, b.tri()&3)
	}
	for i, v := range frame {
		b.start()
		ok := b.writeByte(addr7<<1) && b.writeByte(0x11) && b.writeByte(v)
		b.stop()
		b.restore()
		if !ok {
			return nil, fmt.Errorf("NAK on frame byte %d (%02x)", i, v)
		}
		if b.Mark != nil && i == len(frame)-2 {
			b.Mark("LAST_BYTE_NEXT") // one byte (~90 ms) before the PSU acts
		}
		if b.Mark != nil && i == len(frame)-1 {
			b.Mark("FRAME_SENT") // the PSU has the whole frame now
		}
		time.Sleep(bosGap)
	}
	time.Sleep(bosReply)
	// reply: preamble 55 AA, then LEN, then LEN-1 more bytes
	out := []byte{}
	need := 3
	for len(out) < need && len(out) < 32 {
		if !b.waitIdle(2*time.Millisecond, 2*time.Second) {
			return out, fmt.Errorf("bus busy before reply byte %d", len(out))
		}
		b.start()
		if !b.writeByte(addr7<<1 | 1) {
			b.stop()
			b.restore()
			return out, fmt.Errorf("no ACK on read, byte %d", len(out))
		}
		out = append(out, b.readByte(false))
		b.stop()
		b.restore()
		if len(out) == 3 {
			if out[0] != 0x55 || out[1] != 0xAA || out[2] == 0xF5 || out[2] < 4 {
				return out, fmt.Errorf("bad reply start % x", out)
			}
			need = 2 + int(out[2])
		}
		time.Sleep(bosGap)
	}
	if err := CheckReply(out, c); err != nil {
		return out, err
	}
	return out, nil
}

// code <-> volts from bosminer's own ramp: code 5 = 15.14 V ... code 255 = 11.96 V (12.72 mV/code)
func BosCodeToV(n int) float64 { return 15.14 - float64(n-5)*(15.14-11.96)/250 }

// New maps the PSU's GPIO block read-write.
func New() *Bus { return &Bus{m: MapRW()} }

// NewHalf maps the block with an explicit half-period (older experimental commands).
func NewHalf(h time.Duration) *Bus { return &Bus{m: MapRW(), half: h} }

func (b *Bus) Data() uint32 { return b.data() }
func (b *Bus) Tri() uint32  { return b.tri() }
