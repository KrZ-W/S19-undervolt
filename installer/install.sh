#!/bin/sh
# s19volt installer — puts the S19 heater add-on on top of an existing Braiins OS+ install.
#   ./install.sh <miner-ip>
# Runs from any Linux/macOS PC on the same network (needs ssh + scp). Asks for the miner's root
# password once. Changes nothing in Braiins itself: everything lives in /etc/s19volt + one boot script.
# Safe defaults: power changes and trim are OFF until you calibrate (see docs/PSU-MOD.md).
set -e
M=${1:?usage: ./install.sh <miner-ip>}
here=$(cd "$(dirname "$0")" && pwd)
for f in s19volt s19regs ch3test; do [ -x "$here/bin/$f" ] || { echo "missing bin/$f - run ./build.sh first"; exit 1; }; done

sock=$(mktemp -u /tmp/s19volt-ssh.XXXXXX)
# -n: remote commands must not read this script's stdin (it carries the answers when the install is scripted)
SSH="ssh -n -o ControlPath=$sock -o StrictHostKeyChecking=accept-new root@$M"
SSHIN="ssh -o ControlPath=$sock -o StrictHostKeyChecking=accept-new root@$M"
SCP="scp -O -o ControlPath=$sock"
echo "== connecting to $M (miner root password)"
ssh -fNM -o ControlPath="$sock" -o ControlPersist=600 -o StrictHostKeyChecking=accept-new "root@$M"
trap 'ssh -O exit -o ControlPath=$sock root@$M 2>/dev/null' EXIT

echo "== checking the miner"
# The one supported firmware: Braiins OS+ 25.01 SD (sd-firmware_2025-02-21-0-e05df053-25.01-plus, sha256
# 869ffe82000fd1b76fa717bd3090d1a71eef39fd7a6da97d10644c1f8b4b890b) with its stock bosminer. The per-chip patch
# (bospatch.sh) is made for that exact bosminer build.
WANT_VER=2025-02-21-0-e05df053-25.01-plus
WANT_BOSMINER=10ed4f2707b9ad44f043e52b92ed7b54
ver=$($SSH 'cat /etc/bos_version 2>/dev/null')
bm=$($SSH 'md5sum /rom/usr/bin/bosminer 2>/dev/null | cut -d" " -f1')
echo "   Braiins OS: ${ver:-not found}   bosminer: ${bm:-not found}"
if [ "$ver" != "$WANT_VER" ] || [ "$bm" != "$WANT_BOSMINER" ]; then
  echo "   Unsupported firmware. s19volt supports exactly Braiins OS+ $WANT_VER (stable SD image,"
  echo "   see docs/INSTALL.md) with its stock bosminer - stopping, nothing changed."
  exit 1
fi
$SSH 'pidof bosminer >/dev/null' || { echo "   bosminer is not running - stopping"; exit 1; }
# (2) a fresh Braiins install has no power target: it runs the miner at its full nominal power (~3.2 kW on an S19)
if ! $SSH 'awk "/^\\[autotuning\\]/{s=1;next} /^\\[/{s=0} s && /^power_target *=/{f=1} END{exit !f}" /etc/bosminer.toml'; then
  echo "   Braiins has no power target set: it runs the miner at full power (~3.2 kW on an S19)."
  printf "   Power target in W until the setup wizard has run [1000]: "; read pt; pt=${pt:-1000}
  case "$pt" in *[!0-9]*|"") echo "   not a number - stopping"; exit 1 ;; esac
  $SSH "/etc/init.d/bosminer stop >/dev/null 2>&1; sleep 3; cp -p /etc/bosminer.toml /etc/bosminer.toml.pre-target
    if grep -q '^\[autotuning\]' /etc/bosminer.toml; then
      sed -i '/^\[autotuning\]/,/^\[/{/^mode *=/d}' /etc/bosminer.toml
      sed -i '/^\[autotuning\]/a mode = \"power_target\"\npower_target = $pt' /etc/bosminer.toml
    else printf '\n[autotuning]\nenabled = true\nmode = \"power_target\"\npower_target = $pt\n' >> /etc/bosminer.toml; fi
    /etc/init.d/bosminer start >/dev/null 2>&1; true"
  echo "   power target set to $pt W"
fi

if $SSH 'test -d /etc/s19volt'; then
  # settings only (core.json, web.json, saved tunes lists): the binaries are on your PC and would eat the small
  # overlay (~55 MB on an SD install); one backup is kept, older ones removed
  b=/etc/s19volt.bak-$(date +%Y%m%d-%H%M%S)
  echo "== existing install found: backing up its settings to $b"
  $SSH "/etc/init.d/s19volt stop 2>/dev/null; rm -rf /etc/s19volt.bak-*; mkdir -p $b; cp -p /etc/s19volt/*.json $b/ 2>/dev/null; true"
fi
$SSH 'mkdir -p /etc/s19volt'
$SCP "$here/bin/s19volt" "$here/bin/s19regs" "$here/bin/ch3test" "root@$M:/etc/s19volt/"
$SSH 'chmod 755 /etc/s19volt/s19volt /etc/s19volt/s19regs /etc/s19volt/ch3test'

echo "== trim hardware"
if $SSH '/etc/s19volt/ch3test detect' 2>/dev/null; then
  echo "   4th-connector drive available (wire the optocoupler to it: docs/PSU-MOD.md)."
else
  echo "   this control board is not the tested layout: s19volt will run WITHOUT trim (stock voltage)."
fi

echo "== logins"
printf "   Braiins web/API user [root]: "; read bu; bu=${bu:-root}
[ -t 0 ] && stty -echo; printf "   Braiins web/API password: "; read bp; [ -t 0 ] && stty echo; echo
printf "   Password for the s19volt web UI controls (empty = no password, open on your LAN): "; read uip
printf "   Payout wallet/account to guard against pool hijack (empty = off): "; read wallet
echo "   Live fan control: s19volt sets the fan level itself (fan rule + wizard). This switches Braiins' fan mode to"
echo "   manual (fixed speed set by s19volt; Braiins' hot/dangerous protections stay on). bosminer restarts once."
printf "   Enable live fan control? [Y/n] "; read livefan
case "$livefan" in n|N|no|No) livefan=no ;; *) livefan=yes ;; esac
printf "   Webhook URL for alerts (e.g. Home Assistant; empty = off): "; read hook

json_str() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
cat > /tmp/s19volt-web.$$ <<J
{
 "bos_user": "$(json_str "$bu")",
 "bos_pass": "$(json_str "$bp")",
 "ui_password": "$(json_str "$uip")",
 "levels": []
}
J
cat > /tmp/s19volt-core.$$ <<J
{
 "fan": "shadow", "cap": "shadow", "trim": "off", "power": "off",
 "fan_min": 15, "fan_max": 30, "cap_max": 2200, "trim_cap": 100, "max_amps": 200,
 "levels": {}
}
J
echo "== Braiins auto-upgrade: off (an upgrade would leave the supported build; uninstall.sh turns it back on)"
$SSH 'uci set bos.auto_upgrade.enable=0 && uci commit bos; /etc/init.d/bos_autoupgrade stop >/dev/null 2>&1; /etc/init.d/bos_autoupgrade disable >/dev/null 2>&1; true'
echo "== per-chip data (bosminer patch, RAM only)"
# bosminer 25.01 registers per-chip hashrate/shares/frequency and board voltage in a private metrics registry that
# :8081 never serves. bospatch.sh copies bosminer to RAM, switches those 4 registrations to the served registry and
# mounts the copy over /usr/bin/bosminer, at every boot before bosminer starts. Flash is never changed: remove
# the boot script (uninstall.sh) and reboot = stock bosminer.
$SCP "$here/bospatch.sh" "root@$M:/etc/s19volt/"
$SSHIN "cat > /etc/init.d/s19volt-bospatch" <<I
#!/bin/sh /etc/rc.common
# s19volt: per-chip metrics patch for bosminer 25.01 (RAM copy, bind mount) — installed by install.sh
START=90
start() { sh /etc/s19volt/bospatch.sh; }
stop() { grep -q " /usr/bin/bosminer " /proc/mounts && umount /usr/bin/bosminer; rm -f /tmp/bosminer.patched; true; }
I
$SSH 'chmod 755 /etc/init.d/s19volt-bospatch; /etc/init.d/s19volt-bospatch enable; /etc/init.d/bosminer stop >/dev/null 2>&1; i=0; while pidof bosminer >/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done
      sh /etc/s19volt/bospatch.sh && /etc/init.d/bosminer start >/dev/null 2>&1'
i=0; ok=no
while [ $i -lt 30 ]; do
  sleep 10; i=$((i+1))
  if $SSH 'wget -qO- http://127.0.0.1:8081/metrics | zcat | grep -q "^chip_hashrate{.*type=\"valid\""'; then ok=yes; break; fi
done
if [ "$ok" != yes ]; then
  echo "   per-chip metrics did not appear within 5 min - undoing the patch and stopping."
  $SSH '/etc/init.d/bosminer stop >/dev/null 2>&1; i=0; while pidof bosminer >/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done; /etc/init.d/s19volt-bospatch stop; /etc/init.d/s19volt-bospatch disable; rm -f /etc/init.d/s19volt-bospatch; /etc/init.d/bosminer start >/dev/null 2>&1'
  exit 1
fi
echo "   per-chip hashrate, shares, frequency and board voltage are served."
$SCP "$here/bos-settings.sh" "root@$M:/etc/s19volt/"
$SSH 'sh /etc/s19volt/bos-settings.sh'
if [ "$livefan" = yes ]; then
  sed -i 's/"fan": "shadow"/"fan": "live"/' /tmp/s19volt-core.$$
  echo "== switching Braiins' fan mode to manual (bosminer restarts)"
  # keep the miner's own temperatures; only the mode changes (and sane values are added if missing)
  $SSH 'cp -p /etc/bosminer.toml /etc/s19volt/bosminer.toml.pre-install 2>/dev/null; /etc/init.d/bosminer stop >/dev/null 2>&1; sleep 3
        awk '"'"'
          /^\[/ { if (in_tc && !seen_mode) print "mode = \"manual\""; in_tc = ($0 == "[temp_control]"); seen_mode = 0 }
          in_tc && /^mode *=/ { print "mode = \"manual\""; seen_mode = 1; next }
          { print }
          END { if (in_tc && !seen_mode) print "mode = \"manual\"" }
        '"'"' /etc/bosminer.toml > /tmp/bosminer.toml.new &&
        grep -q "^\[temp_control\]" /tmp/bosminer.toml.new || printf "\n[temp_control]\nmode = \"manual\"\ntarget_temp = 75.0\nhot_temp = 85.0\ndangerous_temp = 95.0\n" >> /tmp/bosminer.toml.new
        mv /tmp/bosminer.toml.new /etc/bosminer.toml; /etc/init.d/bosminer start >/dev/null 2>&1'
  sleep 60
  if $SSH '/etc/s19volt/s19regs fanget' >/dev/null 2>&1; then echo "   live fan control works."
  else echo "   s19regs did not find bosminer's fan setting - stopping (run ./uninstall.sh $M to remove the rest)."; exit 1; fi
fi
if $SSH 'test -f /etc/s19volt/core.json'; then
  echo "   keeping the existing core.json (your calibrated level table)"
else
  $SCP /tmp/s19volt-core.$$ "root@$M:/etc/s19volt/core.json"
fi
$SCP /tmp/s19volt-web.$$ "root@$M:/etc/s19volt/web.json"
rm -f /tmp/s19volt-web.$$ /tmp/s19volt-core.$$
$SSH 'chmod 600 /etc/s19volt/web.json /etc/s19volt/core.json'

args=""
[ -n "$hook" ] && args="$args -webhook $hook -alert $hook"
[ -n "$wallet" ] && args="$args -wallet $wallet"
$SSHIN "cat > /etc/init.d/s19volt" <<I
#!/bin/sh /etc/rc.common
# s19volt (S19 heater add-on) — installed by install.sh; remove with uninstall.sh
START=99
USE_PROCD=1
start_service() {
	procd_open_instance
	procd_set_param command /etc/s19volt/s19volt$args
	procd_set_param respawn 3600 5 0
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
I
# no upgrade keep list on purpose: a Braiins upgrade leaves the supported build, so it removes s19volt (stock miner)
$SSH 'chmod 755 /etc/init.d/s19volt; rm -f /lib/upgrade/keep.d/s19volt
      /etc/init.d/s19volt enable; /etc/init.d/s19volt restart 2>/dev/null || /etc/init.d/s19volt start'

echo "== self-test"
sleep 5
if $SSH 'wget -qO- http://127.0.0.1:8090/api/status >/dev/null'; then
  echo "   s19volt is running."
else
  echo "   s19volt did not answer - check: ssh root@$M logread -e s19volt"; exit 1
fi
cat <<D

Done. Web UI: http://$M:8090/
Power changes and the trim loop are OFF until you calibrate
(floor check + efficiency search: docs/PSU-MOD.md). Fan rule and temperature cap run in
shadow mode (they log what they would do).
Supported firmware: Braiins OS+ $WANT_VER only. Do not upgrade Braiins: s19volt and the
per-chip patch are made for this exact build.
D
