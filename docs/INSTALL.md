# Installing and using the software

This guide covers the software side: building, installing, the setup wizard and daily use. Build the hardware mod
first: [PSU-MOD.md](PSU-MOD.md). Without the mod, the software still runs, but only at stock voltage.

Items marked **not built** or **untested** are exactly that. Do not rely on them.

## Requirements

**On the miner**

- Antminer S19 with an APW121215 PSU and the trim mod ([PSU-MOD.md](PSU-MOD.md)).
- A Xilinx Zynq control board. Tested: AMCB07_4X1_4F_S_VER2.3. See
  [control board compatibility](PSU-MOD.md#control-board-compatibility).
- **Braiins OS+**, with `bosminer` running and the miner hashing.
  - Tested: `2026-10-19-0-b3bb8db3-26.11-plus-nightly`, platform `zynq-bm3-am2`.
  - Stable 26.10: expected to work (same REST endpoints), not tested yet.
- Root SSH access to the miner (the Braiins root password).
- The Braiins web/API login (the installer asks for it; `s19volt` uses the local REST API).

**On your PC**

- Linux or macOS with `ssh` and `scp`. Windows: WSL or Git Bash should work — **untested**.
- Go (only to build the binaries).
- The miner on the same network.

## Boot medium: SD card or internal NAND

Braiins OS+ runs either from an SD card or from the control board's internal NAND flash. Both keep `/etc` on a small
writable overlay. On the test miner (SD boot) that overlay is only **~55 MB**, and the `s19volt` binary alone is
about 7 MB. A full overlay breaks an install half-way.

The installer is meant to detect the boot medium and check the free space before it copies anything.

- TODO: boot medium detection and the free-space check are **not yet in `install.sh`**. Until they are, check by hand
  first: `ssh root@<miner-ip> df -h /etc` — keep at least 25 MB free.
- TODO: free space on a NAND install not yet measured.

Never keep extra copies of the binaries (rollback copies, old builds) on the miner. Keep them on your PC.

## Building

```
cd installer
./build.sh
```

This cross-compiles three ARM binaries (`GOOS=linux GOARCH=arm GOARM=7`, static) into `installer/bin/`:

| Binary | What |
|---|---|
| `s19volt` | the service: trim, levels, fans, web page, setup wizard |
| `ch3test` | meter tests for the wiring (`detect`, `toggle`, `high`, `pwm`, `low`, `hold`) |
| `s19regs` | register tool, used to set the fan speed live |

## Installing

The installer is a shell script. It runs on your PC and talks to the miner over SSH.

```
cd installer
./install.sh <miner-ip>
```

It asks for the miner's root password once (one shared SSH connection), then:

1. **Checks the miner.** Stops unless the firmware is Braiins OS+ (`/etc/bos_version` contains `plus`), the platform
   is `zynq-*` (`/etc/bos_platform`), and `bosminer` is running.
2. **Backs up an existing install** to `/etc/s19volt.bak-<date>` and stops the old service.
3. **Copies the binaries** to `/etc/s19volt/`.
4. **Checks the trim output** with `ch3test detect`. If the control board is not the tested layout, `s19volt` runs
   **without trim** (stock voltage only).
5. **Asks for logins and options:**
   - Braiins web/API user and password.
   - Password for the `s19volt` controls. Empty = no password: anyone on your LAN can change the level.
   - Payout wallet/account to guard against a pool hijack (empty = off).
   - Live fan control (default yes, see below).
   - Webhook URL for alerts, e.g. a Home Assistant webhook (empty = off).
6. **Writes the config** and the boot script, enables and starts the service.
7. **Self-test:** checks that `http://127.0.0.1:8090/api/status` answers on the miner.

### What the installer changes

| Where | What |
|---|---|
| `/etc/s19volt/` | binaries, `core.json` (level table + loop settings), `web.json` (logins; mode 600) |
| `/etc/init.d/s19volt` | boot script (procd, start priority 99, respawn) |
| `/etc/bosminer.toml` | only with live fan control: `mode = "manual"` in `[temp_control]` (see below) |
| `/etc/s19volt/bosminer.toml.pre-install` | copy of `bosminer.toml` before that change |
| `/lib/upgrade/keep.d/s19volt` | asks the firmware to keep the files above across upgrades — **untested** |

Nothing else in Braiins OS+ is changed. An existing `core.json` is kept, so a reinstall keeps your level table.

**Live fan control.** `s19volt` sets the fan speed itself (fan rule and wizard). For that, Braiins' fan mode must be
**manual** (a fixed speed, which `s19volt` then moves). The installer sets `mode = "manual"` in the `[temp_control]`
section of `/etc/bosminer.toml`, keeps your temperature values, and adds sane ones if the section is missing.
Braiins' hot and dangerous temperature protections stay on. `bosminer` restarts once. The installer then checks that
`s19regs` finds the fan setting. If it does not, fans stay under Braiins' control.

**Safe start.** After a fresh install, power changes and the trim loop are **off**. The fan rule and the temperature
cap run in shadow mode (they log what they would do). The setup wizard turns them on when you apply its table.

## First start

Open `http://<miner-ip>:8090/`. This is the **heater page**: status, 24 h history, hashboards, fans, controls,
efficiency chart. If you set a controls password, unlock the controls with it.

Then check the wiring by hand once (see [PSU-MOD.md, Verifying](PSU-MOD.md#verifying)) and open the setup page:
**Setup / calibration →** on the heater page, or `http://<miner-ip>:8090/setup.html`.

## The setup wizard

The wizard measures your miner, your PSU and your mod, and writes the level table the heater uses: for each power
level, the board count, the board voltage, the trim drive and the fan.

> The wizard is new. It has not yet completed a full run on hardware. Watch the first run.

### Inputs ("What you want")

| Input | Meaning |
|---|---|
| **Wattage range** + **step** | The power levels your heating uses, in estimated watts at the wall. One level per step, optimised from low to high power. Levels are never set below Braiins' own minimum power target (read from the miner). |
| **Maximum fan** + **Listen 30 s** | The loudest the miner may get. *Listen* runs the fans at that speed for 30 s so you can hear it from where you sit. The minimum is fixed at 15 %. Default 25 %. |
| **Chip temperature** | Target, recommended 74 °C. The S19 reports an estimate (board sensor + 15 °C), not a real chip reading. |
| **Voltage margin** | Added to each level's answer in the table. 0 = as measured. |
| **Test fewer boards at low power** | Ranks the boards at low voltage first, then adds 2-board levels below 600 W and 1-board levels below 400 W (also below your range, down to 300 W) using the strongest boards. Keeps the most efficient board count per level. |
| **I understand** (required) | Your saved Braiins tunes are moved to a backup before the efficiency search, so every level is tuned fresh. |

During the whole run the fan rule drives the fans live between 15 % and your maximum: up 5 % when chips reach 74 °C,
down 2 % when they stay at or below 70 °C, at most one move a minute. Chips are measured at the temperature they will
run at.

The page shows a time estimate before you start. A full run takes many hours; the worst case at the default range is
up to ~54 h. An interruption means starting again (there is no resume).

**While the wizard runs, outside commands are refused.** `/api/level` and the heater page controls (level, power,
trim, fan, boards) answer `409 {"error": "setup running"}`. Pause your thermostat automation so it does not keep
trying.

### Stages

Each stage has a **?** button with this help on the page.

1. **Check the miner.** Checks the miner, the Braiins API, the boards and the trim output. Reads Braiins' minimum
   power target. About 3 min.
2. **Fixed test profile.** Switches Braiins to manual mode: tuner off, the same conservative clock (~250 MHz, about
   900–1000 W) on every chip, at Braiins' minimum PSU voltage (read from bosminer, else 11.96 V). The low clock is on
   purpose: high clocks at low voltage can latch boards. Waits until every board hashes, the chips have reached the
   profile clock and the PSU has stopped stepping (voltage readings are wrong while it steps); about 3 min. If manual
   mode is refused, the wizard falls back to letting Braiins tune the bottom of your range (about 30 min). The miner
   is always put back in tuner mode at its previous power target, even if the setup stops or fails.
3. **Trim sweep 0-100 % (mod check, drive chart, lowest voltage).** Sweeps the trim from 0 to 100 % in 2 % steps
   and reads the board voltage about 3 s after each step (the capacitor on the LED drive lags ~3 s). It is also the
   trim test: as soon as the voltage is 0.15 V below the 0 % reading, the page shows e.g. "✓ the mod works:
   11.96 → 11.79 V at 34 %" while the sweep goes on. If the voltage has not dropped by 0.15 V at 60 %, the sweep
   stops and the stage fails ("the mod is not acting on the voltage — check the wiring"). Every point is the drive
   chart on the results card: where the mod starts acting, the useful range, and the floor your series resistor
   sets (the mean board voltage at 100 %, about 8.8 V with 33 kΩ). The sweep always runs to 100 %, even when boards
   stop hashing: at the test clock that says nothing about your low-power levels, so the chart and the stage only
   show it as information ("boards stop hashing at X % (Y V) at the test profile"). It is measured from Braiins'
   minimum voltage, the same setpoint the tuned levels trim from, so drive % → voltage carries over. No later step
   goes near the floor. About 3 min, plus the glide back to 0 % (about 3 min).
4. **Back to Braiins' tuner.** Puts Braiins back in tuner mode at your previous power target (bosminer restarts,
   which clears any chip the deep end of the sweep latched), then moves your saved Braiins tunes to a backup. About
   6 min.
5. **Efficiency search.** For each level, from low to high power:
   - a soft reset: pause, cool, bosminer off and on with the trim at 0 (clears latched chips);
   - a fresh Braiins tune (~20 min); the 5-min stock baseline is measured while waiting for Braiins' settled
     stage-4 round (a sliding window that any Braiins voltage change restarts), so trimming starts as soon as the
     round is settled and 5 clean minutes are collected;
   - trim drive steps, each a 5-min hold with no voltage corrections, judged against the baseline: a step passes
     with at least 96 % of the baseline TH/s and at most 1 newly weak chip; a failing step is followed by a soft
     reset. TH/s (baseline and holds) are exact window averages from bosminer's own hash counters ("Total MH" in
     the cgminer API on port 4028; the 1-min board rates only if that port does not answer). Per-chip data is the
     stock chip_hashrate metric averaged every 30 s over the window. A step's voltage is the hold's average board voltage (single readings refresh only every ~10 s per
     board and swing too much to steer on). Step sizes are 0.2 / 0.1 / 0.05 V converted to drive at the local slope;
   - the answer is the passing step with the most drive (lowest voltage), then a latch check (latching only gets worse at
     lower voltage): if the last step ran at or below the answer's voltage, straight back to stock and compare with
     the baseline (clean = cleared; latched after a step below the answer = inconclusive: soft reset, 5 min at the
     answer, check again); if it ran above, 5 min at the answer first (a soft reset before it if a lower step ran
     since the last one). If chips stay
     weak there, 0.05 V less drive is tested again (up to 4 times); after that the level runs at stock voltage.

   About 40 min per level, worst case a few hours.
6. **Level table.** Writes the table (voltage, drive and fan per level). Instant.

The **Progress** card shows the live step, Braiins' tuner state (stage, round, per-board clock), the stage list, the
last events and a **Cancel run** button. Cancel returns the miner to its previous level at stock voltage; results so
far are kept.

### Results and applying

The **Results** card shows, per level: board count, status, Braiins target, stock and answer voltage, drive, TH/s,
J/TH stock → trimmed, chip temperature and fan. Wall watts are **estimated** from a power model fitted on a metered
S19 (typically within ~10–25 W), not measured. The efficiency curve and the drive chart are drawn from the run.

The **Chip map** shows how much work each chip kept at each voltage step. It needs per-chip data; see
[troubleshooting](#troubleshooting).

**Apply table** writes the table to `/etc/s19volt/core.json`. The old table is backed up first
(`/etc/s19volt/core.json.pre-wizard-*`). Leave "s19volt holds each level's voltage itself" ticked unless another
system runs the trim loop.

### Your saved Braiins tunes

Stage 6 moves Braiins' saved tunes to `/etc/s19volt/backup/bosminer-autotune-<date>.json`. They are moved, not
deleted. The stage detail on the setup page shows the exact path. To put them back:

```
ssh root@<miner-ip>
/etc/init.d/bosminer stop
cp /etc/s19volt/backup/bosminer-autotune-<date>.json /etc/bosminer-autotune.json
/etc/init.d/bosminer start
```

## Daily use

The heater page (`http://<miner-ip>:8090/`) shows the state and offers:

- **Mode:** *Manual* — the level is set through the API (thermostat) or on this page. *Maximum efficiency* — runs
  the lowest-J/TH level. *Maximum power the cooling allows* — today simply the highest table level under the
  temperature cap; a dynamic version that follows chip temperature at maximum fan is **not built** yet.
- **Power:** pick a level. The change is burst-free: trim to 0 → pause → switch boards → power target → cool →
  resume → every board hashing → glide the trim to the level's voltage. Board switches restart bosminer (~1 min).
- **Boards, Fan, Trim, Pause/Resume:** manual controls for testing.
- A banner shows who controls the level: *Setup running*, *API (last request N min ago, from <address>)*, or *Manual*.

### Thermostat and automation API

| Call | What |
|---|---|
| `POST /api/level` `{"watts": N}` | Request a level, N from 300 to 3000. Rounded **down** to the nearest level in the table, never above the temperature cap. The reply carries `effective_target`. Below the lowest level, the lowest level runs (more than asked). |
| `GET /api/core` | Mode, requested and effective level, current phase, level table. |
| `POST /api/core` `{"mode": "manual"\|"efficiency"\|"max", "save": true}` | Change the mode. |
| `GET /api/status` | Everything the heater page shows. |
| `GET /api/history` | 30-s samples, last 24 h (in memory). |

If a controls password is set, send it in the `X-Auth` header. While the setup wizard or a calibration runs, control
calls answer `409 {"error": "setup running"}`.

Example:

```
curl -X POST http://<miner-ip>:8090/api/level -H 'Content-Type: application/json' -d '{"watts": 800}'
```

There is no "pause" through the API yet. A thermostat that wants no heat can request the lowest level or cut power.

## Firmware upgrades

The installer lists `/etc/s19volt/` and `/etc/init.d/s19volt` in `/lib/upgrade/keep.d/s19volt`, the standard
OpenWrt keep list. Whether Braiins upgrades honour it is **untested**.

After an upgrade:

1. Check the heater page answers.
2. If it does not, run `./install.sh <miner-ip>` again. Your `core.json` level table is kept.
3. If you use live fan control, check `[temp_control]` in `/etc/bosminer.toml` still says `mode = "manual"`. The
   installer sets it again.

## Uninstalling

```
cd installer
./uninstall.sh <miner-ip>
```

It stops the service, forces the trim output LOW (stock voltage), disables and removes the boot script and the keep
list, and moves `/etc/s19volt` to `/etc/s19volt.removed-<date>`. Braiins OS+ itself is untouched.

Not undone: the fan mode in `/etc/bosminer.toml`. To restore it, copy
`/etc/s19volt.removed-<date>/bosminer.toml.pre-install` back to `/etc/bosminer.toml` and restart bosminer, or set
the fan mode in the Braiins web UI. Saved tunes moved by the wizard stay in `/etc/s19volt.removed-<date>/backup/`.

The hardware mod can stay in place: with the output LOW, the PSU runs at stock voltage.

## Troubleshooting

**Where is the setup page?** On the heater page, **Setup / calibration →** (next to the mode selector), or
`http://<miner-ip>:8090/setup.html`.

**Fans do not follow the fan rule or the wizard.** Braiins must be in manual fan mode. Check `[temp_control]` in
`/etc/bosminer.toml` for `mode = "manual"`, or re-run the installer and answer yes to live fan control. Note that a
bosminer restart resets the fan to the configured value until `s19volt` applies its own again.

**The trim sweep fails with "the mod is not acting on the voltage"** (no 0.15 V drop by 60 % trim). Check the wiring: run `ch3test toggle` and measure the
output pin, check the PC817 orientation on both sides, the 4.7 kΩ resistor, and the leads on R200. See
[PSU-MOD.md](PSU-MOD.md#verifying).

**`ch3test detect` says "not the tested layout".** The control board or firmware does not match what the software
checks for. It refuses to drive anything, so the miner stays at stock voltage. See
[control board compatibility](PSU-MOD.md#control-board-compatibility).

**A power target is not accepted.** Braiins has a minimum power target (~360 W on the test miner); the software
reads it from `GET /api/v1/configuration/constraints` and clamps levels to it. A level change retries the power
target until bosminer accepts it. If it never does, check the Braiins login in `/etc/s19volt/web.json` and the
bosminer log (`logread -e bosminer`).

**Per-chip data on stock firmware.** Stock Braiins OS+ publishes the `chip_hashrate` gauge; `s19volt` averages it
every 30 s over each window (checked offline against counted shares: about 1 % median error on 5-min windows, the
same weak-chip verdicts). A bosminer that also publishes `chip_shares` counters is used through those instead
(level field `chip_src`: `hashrate` or `shares`).

**The service does not answer.** `ssh root@<miner-ip> logread -e s19volt`.

**bosminer's power figure looks wrong.** It assumes stock voltage and is wrong while trimmed. Measure at the wall.
