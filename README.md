# S19 undervolt

Run an Antminer S19 efficiently at low power, as a space heater. Braiins OS+ will not ask the APW12 PSU for less
than **11.96 V**. At low power that wastes energy: the chips could run at a much lower voltage. This project adds a
small hardware mod and a service on the control board that **trim the PSU below that minimum, live, while mining**.
The boards go down to ~9.5 V. Result: about **22–24 J/TH between 300 and 1500 W at the wall**, against ~33 J/TH at
stock voltage for the same heat.

- **Hardware:** an optocoupler, two resistors and a capacitor (under $2), driven by the miner's own control board.
  No microcontroller, no Wi-Fi.
- **Software:** `s19volt`, a small service that runs on the control board next to Braiins OS+. It holds the voltage,
  changes power levels without voltage spikes (including 1- and 2-board levels), drives the fans, caps chip
  temperature, and has a web page and a setup wizard. A thermostat only chooses the level.

> [!WARNING]
> **Mains voltage.** The mod is soldered inside the APW12 PSU. Its bulk capacitors hold ~400 V DC for minutes
> after unplugging. Unplug, wait, and check with a meter before touching anything inside. A wrong connection on the
> feedback network can send the PSU to its maximum voltage and kill the hashboards. Everything here is at your own
> risk.

## Results

S19, measured at the wall on Braiins OS+ 26.11 (before the move to 25.01; a 25.01 run will replace this table). Each level at its best stable voltage, checked for latched chips.

| Setup | Wall | TH/s | J/TH | Board V |
|---|---|---|---|---|
| 1 board, 240 MHz | 270 W | 11.1 | **24.4** | 9.70 |
| 2 boards, 200 MHz | 417 W | 18.1 | 23.0 | 9.48 |
| 2 boards, 240 MHz | 496 W | 22.0 | **22.6** | 9.70 |
| 3 boards, 800 W level | 633 W | 28.3 | **22.4** | 9.72 |
| 3 boards, 1000 W level | 831 W | 36.8 | 22.6 | 10.10 |
| 3 boards, 1400 W level | 1222 W | 52.2 | 23.4 | 10.65 |
| 3 boards, 2000 W level | 2038 W | 75.2 | 27.1 | 11.54 |
| *stock reference, 3 boards, 600 W level* | *690 W* | *20.9* | *33.0* | *11.82* |

These figures were measured with the first version of the mod (V1, optocoupler driven by an ESP32). The current
version (V2, driven by the control board) reaches the same voltages. Below ~560 W at the wall, 2 boards (or 1) beat
3 boards at the same heat; the software switches boards for those levels.

## What you need

- An Antminer S19 with an **APW121215** PSU (12–15 V family, revisions a–g).
- A **Xilinx Zynq** control board with an empty 4th hashboard connector (tested: AMCB07_4X1_4F_S_VER2.3).
- **Braiins OS+ 25.01** (stable SD image, the only supported firmware; see [INSTALL.md](docs/INSTALL.md#requirements)).
- Parts: PC817 optocoupler, 33 kΩ and 4.7 kΩ resistors, 470–1000 µF capacitor, two Dupont wires.
- A soldering iron, a multimeter, and a Linux or macOS PC with Go to build the software.

## Quick start

1. **Build the mod** — [docs/PSU-MOD.md](docs/PSU-MOD.md): safety, how it works, parts, wiring, checks.
2. **Install the software** — [docs/INSTALL.md](docs/INSTALL.md): build, install, what changes on the miner.
3. **Run the setup wizard** — open `http://<miner>:8090/setup.html`. It proves the mod works within minutes, then
   finds the best voltage for every power level and writes the level table
   ([walkthrough](docs/INSTALL.md#the-setup-wizard)).

After that, a thermostat or an automation sets the level with `POST /api/level {"watts": N}`
([daily use](docs/INSTALL.md#daily-use)).

## Repository layout

| Path | What |
|---|---|
| `cmd/s19volt/` | the service, web page and setup wizard (`ui/`) |
| `ch3/` | the control-board output: board check by address, sysfs PWM |
| `apw12/` | APW12 PSU I2C protocol |
| `cmd/ch3test/` | meter tests for the wiring (`detect`, `toggle`, `low`, …) |
| `s19regs/` | read-mostly FPGA/chip register tool (used for the fan setting) |
| `installer/` | `build.sh`, `install.sh <miner-ip>`, `uninstall.sh <miner-ip>` |
| `core.example.json` | example level table |
| `docs/` | [PSU-MOD.md](docs/PSU-MOD.md) (hardware), [INSTALL.md](docs/INSTALL.md) (software) |

## Compatibility

| Part | Status |
|---|---|
| PSU APW121215 (a–g) | in scope; tested on one unit |
| PSU APW121417 (14–17 V) | untested, different range |
| Control board Xilinx Zynq AMCB07_4X1_4F_S_VER2.3 | tested |
| Control board Xilinx Zynq C87 | should work; verify with `ch3test` |
| Other Zynq boards (C52/C55/C71) | unknown; `ch3test` refuses on a mismatch |
| BeagleBone, Amlogic, CVITEK, BCB100 | not supported |
| Braiins OS+ 25.01 (stable SD, bosminer md5 `10ed4f27…`) | supported (the installer refuses anything else) |
| Any other Braiins OS+ version | not supported |
| Stock Antminer firmware, other firmwares | not supported (the output pin comes from Braiins' FPGA design) |

Details: [PSU compatibility](docs/PSU-MOD.md#psu-compatibility) and
[control board compatibility](docs/PSU-MOD.md#control-board-compatibility).

## Status and roadmap

- **V1 (retired):** the optocoupler's LED was driven by an ESP32's DAC.
- **V2 (current):** the control board drives the optocoupler itself. Hardware in daily use on one miner. The setup
  wizard is new and has not yet completed a full run on hardware.
- **V3 (idea, on hold):** rebuild on open firmware instead of on top of Braiins OS+. D-Central's DCENT_OS (GPL-3.0)
  was checked in 2026-11: no released S19 image yet, and on Zynq S19s it still boots Braiins' boot chain, kernel and
  FPGA design, so for now it would add a layer rather than remove one.

As far as we could find, no feedback-network undervolt of the APW12 has been published before this one.

## License

License: TBD
