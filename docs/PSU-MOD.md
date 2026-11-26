# The PSU trim mod (hardware)

This guide covers the hardware: what the mod does, the parts, how to build and check it, and what it works with.
The software side is in [INSTALL.md](INSTALL.md).

## Safety

> [!WARNING]
> **Mains voltage.** The APW12 has **~400 V DC on its bulk capacitors for minutes after unplugging**. Unplug it,
> wait, and check with a meter across the bulk capacitors before touching anything inside. Keep the new wires away
> from the high-voltage side.
>
> **Wrong wiring can kill the hashboards.** A bad connection on the feedback network can send the PSU to its maximum
> output voltage. Check every joint before closing the PSU.
>
> You do this at your own risk.

## How it works

The APW12 regulates its output with a resistor divider in its feedback loop. **R200** is one resistor of that
divider. Extra resistance **in parallel with R200** changes the divider ratio and lowers the output voltage. The
lower the parallel resistance, the lower the voltage.

The mod puts a **fixed resistor in series with an optocoupler transistor** across R200. The optocoupler's LED is
driven by a spare output of the miner's control board:

```
  PSU side                                         control board side (empty 4th hashboard connector)
             R200 (feedback divider)
      ┌────────────/\/\/\────────────┐             pin 16 (reset line) ──[4.7 kΩ]──┬──► PC817 pin 1 (LED +, dot)
      │                              │                                             │
      │   ┌──── PC817 C ──── E ───┐  │                                        470–1000 µF  (+ on pin 1 side)
      └───┤                       ├──┘                                             │
          │  33 kΩ                │                 pin 14 (GND) ──────────────────┴──► PC817 pin 2 (LED −)
          └────────/\/\/\─────────┘
```

- **LED dark (0 %)**: the transistor is off, nothing is across R200, the PSU gives **stock voltage**.
- **LED driven**: the transistor conducts, the resistance across R200 drops, the output voltage drops.
- **The output pin.** The S19 uses three of the control board's four hashboard connectors. The empty fourth one
  carries that slot's **reset line**, which nothing uses while the slot is empty. It is an ordinary FPGA output in
  Braiins' design (AXI GPIO at 0x41210000, bit 3; bits 0–2 are the three boards' resets). It is wired to the
  connector through a 4.7 kΩ resistor on the board.
- **Boot default LOW = stock voltage.** The pin is LOW at power-on, and the software starts at 0 %. Every boot,
  crash or power loss lands at stock voltage, never at an unexpected low.
- **PWM and capacitor.** The pin is either on or off. The software drives it with **PWM at 1 kHz**, and the
  capacitor averages that into a steady LED current.
- **Fail-safe fade.** The capacitor is large on purpose. If the software or the control board dies, the trim **fades
  out over a few seconds** instead of snapping back to stock voltage under load.
- **Isolation.** The optocoupler keeps the control board galvanically isolated from the PSU's feedback network.

## Parts

| Part | Notes |
|---|---|
| PC817 optocoupler | any PC817; one from a scrapped 5 V USB charger works |
| 33 kΩ resistor | series resistor, PSU side — colour code **orange-orange-orange** |
| 4.7 kΩ resistor | LED current limit — colour code **yellow-violet-red** |
| 470–1000 µF electrolytic capacitor | rated **6.3 V or more** |
| 2 female Dupont jumper wires | fit the 2.54 mm control-board connector |
| short thin wire | from R200 to the optocoupler; twist the pair |

Tools: soldering iron, multimeter, screwdrivers for the PSU case.

## Choosing the series resistor

The series resistor sets the **floor**: the lowest voltage the mod can reach at 100 % drive. Measured at 450 W:

| Series resistor | Floor |
|---|---|
| 15 kΩ | 7.1 V |
| 20 kΩ | 7.9 V |
| **33 kΩ** | **8.8 V** |

**Use 33 kΩ.** The APW12 is specified for 12–15 V. It has run for days at 9.5–10 V, but nothing is known about
7–8 V. With 33 kΩ, even a pin stuck at 100 % only reaches ~8.8 V, and the boards were still partly hashing there.
The best levels sit around 9.5–11.5 V, well above that floor.

### Drive vs voltage (33 kΩ, 450 W, 2 boards)

The response is not linear. Nothing happens below ~30 % drive, then the voltage falls faster and faster.

| Drive % | 0–30 | 40 | 50 | 55 | 60 | 65 | 75 | 85–100 |
|---|---|---|---|---|---|---|---|---|
| Board V | 11.82 | 11.7 | 11.45 | 11.1 | 10.5 | 9.8 | 9.1–9.4 | 8.8 (floor) |

The curve shifts with load and setpoint. The software does not rely on a fixed table: it measures the curve in the
setup wizard and holds each level's voltage by feedback.

## Building it

### PSU side

1. Unplug the PSU. Wait. Check the bulk capacitors are discharged with a meter.
2. Open the APW12 and find **R200** in the output feedback network.

   ![R200 location](img/r200.jpg) — TODO photo

3. Solder two short leads to the two ends of R200.
4. Wire the 33 kΩ resistor and the PC817's collector–emitter (pins 4 and 3) **in series** between those two leads.
   Mind the orientation: collector (pin 4) towards the R200 end at the higher potential.
5. Bring the PC817's LED pins (1 and 2) out of the case on two wires for the control board.
6. Check every joint, insulate, and close the PSU.

### Control-board side

Do the wiring **with the miner unpowered**. A discharged capacitor touching the 3.3 V pin resets the control board.

The connector is 18 pins (2×9, 2.54 mm). Pin numbers follow the S9 control-board schematic, which matches this
board. The physical row order on your board may look swapped, so **identify the pins with a meter** (step 8).

| Pin | Signal |
|---|---|
| 1, 2, 9, 10, 13, 14 | GND |
| 3, 4 | I2C, shared with the working boards — **do not touch** |
| 11, 12 | serial RX/TX (also used as a fan-speed input) — **do not drive** |
| 15 | 3.3 V |
| **16** | **reset line = the trim output** |

![CH3 pins 16 and 14](img/ch3-pins.jpg) — TODO photo

7. Install the software first ([INSTALL.md](INSTALL.md)); it includes `ch3test`.
8. Find the output pin. On the miner, stop the service first (`/etc/init.d/s19volt stop`: two programs must not
   drive the pin at once), then run `/etc/s19volt/ch3test toggle`. Put the meter's black probe on a GND pin
   (it beeps to the chassis) and probe the empty connector. The pin that **swings 0 ↔ 3.3 V every 5 s** is the trim
   output (pin 16). Stop `ch3test` (it leaves the pin LOW) and power the miner off.
9. Wire it:
   - output pin (16) → 4.7 kΩ → PC817 pin 1 (the dot);
   - PC817 pin 2 → a GND pin (14);
   - capacitor across PC817 pins 1 and 2, **+ on the pin 1 side**.
10. Power on. `s19volt` starts at boot with the pin LOW.

## Verifying

1. **`ch3test`** (with `s19volt` stopped, low power level). `ch3test detect` prints the detected pin and changes nothing. `ch3test pwm -duty 50 -secs 60`
   drives the LED through the capacitor; the board voltage should drop. `ch3test low` forces the pin LOW. Then `/etc/init.d/s19volt start`.
2. **The wizard's trim sweep.** It steps the trim from 0 to 100 % in 2 % steps and records the voltage at each
   step. A drop of 0.15 V below the 0 % reading shows "✓ the mod works"; no such drop by 60 % stops it with "check
   the wiring". The 100 % point is the lowest voltage: with 33 kΩ it should settle around 8.8 V. If it goes much
   lower, check the series resistor before going further. See [INSTALL.md, The setup wizard](INSTALL.md#the-setup-wizard).

If the voltage does not move: check the output pin with `ch3test toggle`, the PC817 orientation on both sides, the
4.7 kΩ resistor, and the leads on R200.

## Limits

- **Never combine high power with low voltage.** Output current = power ÷ voltage. 3000 W at 7 V would be ~430 A,
  far over the APW12's ~250 A: an over-current trip at best, a dead PSU at worst. The software refuses to trim below
  `watts ÷ 200 A` (`max_amps` in `core.json`). Do not raise it.
- **Fail-safe = stock voltage.** If `s19volt` stops, the pin goes LOW and the trim fades out over a few seconds:
  more power, more heat. The software's chip-temperature cap covers that.
- **Change voltage slowly.** A jump in voltage under load can drop boards at high power. The software moves the trim
  in glides (5 % per 10 s).
- **Latched chips.** Going below a level's floor can leave chips dead until the boards lose power. The software
  clears them with a soft power cycle (bosminer stop and start), and its searches step down from above the floor.
- bosminer's own power figure is wrong while trimmed (it assumes stock voltage). Measure at the wall.

## PSU compatibility

- **APW121215** family (12–15 V output, revisions a–g): in scope. Tested on one unit.
- **APW121417** (14–17 V output, used in the L7 and some S19 XP): different range, **untested**.
- No public source names R200 or the feedback divider for any revision. **Find the R200-equivalent on your own
  board**: the resistor in the output voltage feedback divider. Check the board revision before you solder.

## Control board compatibility

The output pin comes from Braiins' FPGA design, so the mod needs **Braiins OS+**. The software finds the GPIO block
by address (never by Linux GPIO number), checks its width and boot default, and checks that plug-detect says the 4th
connector is empty. If anything does not match, it refuses to drive the pin. Refusing means stock voltage.

| Board | 4th connector | Status |
|---|---|---|
| Xilinx Zynq AMCB07_4X1_4F_S_VER2.3 | yes | tested |
| Xilinx Zynq C87 | yes ("4 x I/O headers") | should work — verify with `ch3test detect` / `toggle` |
| Other Zynq (C52/C55/C71) | unknown | verify with `ch3test` (it refuses on a mismatch) |
| BeagleBone, Amlogic, CVITEK, BCB100 | — | not supported |

Tested firmware: Braiins OS+ `2026-10-19-0-b3bb8db3-26.11-plus-nightly`, platform `zynq-bm3-am2`.

## History

- **V1:** the optocoupler's LED was driven by an ESP32's DAC. The results table in the [README](../README.md) was
  measured with it.
- **V2 (current):** the control board drives the optocoupler itself. It reaches the same voltages.
