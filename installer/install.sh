#!/bin/sh
# s19volt installer — puts the S19 heater add-on on top of an existing Braiins OS+ install.
#   ./install.sh <miner-ip>
# Runs from any Linux/macOS PC on the same network (needs ssh + scp). Asks for the miner's root
# password once. Changes nothing in Braiins itself: everything lives in /etc/s19volt + one boot script.
# Safe defaults: power changes and trim are OFF until you calibrate (see docs/PSU-TRIM-MOD.md).
set -e
M=${1:?usage: ./install.sh <miner-ip>}
here=$(cd "$(dirname "$0")" && pwd)
for f in s19volt s19regs ch3test; do [ -x "$here/bin/$f" ] || { echo "missing bin/$f - run ./build.sh first"; exit 1; }; done

sock=$(mktemp -u /tmp/s19volt-ssh.XXXXXX)
SSH="ssh -o ControlPath=$sock -o StrictHostKeyChecking=accept-new root@$M"
SCP="scp -O -o ControlPath=$sock"
echo "== connecting to $M (miner root password)"
ssh -fNM -o ControlPath="$sock" -o ControlPersist=600 -o StrictHostKeyChecking=accept-new "root@$M"
trap 'ssh -O exit -o ControlPath=$sock root@$M 2>/dev/null' EXIT

echo "== checking the miner"
ver=$($SSH 'cat /etc/bos_version 2>/dev/null')
plat=$($SSH 'cat /etc/bos_platform 2>/dev/null')
echo "   Braiins OS: ${ver:-not found}   platform: ${plat:-?}"
case "$ver" in *plus*) ;; *) echo "   not Braiins OS+ - stopping"; exit 1 ;; esac
case "$plat" in zynq-*) ;; *) echo "   only Zynq control boards are supported for now - stopping"; exit 1 ;; esac
$SSH 'pidof bosminer >/dev/null' || { echo "   bosminer is not running - stopping"; exit 1; }
echo "   tested: 2026-10-19 26.11-plus-nightly on zynq-bm3-am2 (other versions: should work, not tested)"

if $SSH 'test -d /etc/s19volt'; then
  b=/etc/s19volt.bak-$(date +%Y%m%d-%H%M%S)
  echo "== existing install found: backing it up to $b"
  $SSH "cp -a /etc/s19volt $b; /etc/init.d/s19volt stop 2>/dev/null || true"
fi
$SSH 'mkdir -p /etc/s19volt'
$SCP "$here/bin/s19volt" "$here/bin/s19regs" "$here/bin/ch3test" "root@$M:/etc/s19volt/"
$SSH 'chmod 755 /etc/s19volt/s19volt /etc/s19volt/s19regs /etc/s19volt/ch3test'

echo "== trim hardware"
if $SSH '/etc/s19volt/ch3test detect' 2>/dev/null; then
  echo "   4th-connector drive available (wire the optocoupler to it: docs/PSU-TRIM-MOD.md)."
else
  echo "   this control board is not the tested layout: s19volt will run WITHOUT trim (stock voltage)."
fi

echo "== logins"
printf "   Braiins web/API user [root]: "; read bu; bu=${bu:-root}
stty -echo; printf "   Braiins web/API password: "; read bp; stty echo; echo
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
 "bos_url": "http://127.0.0.1/api/v1",
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
  else echo "   WARNING: s19volt could not find the fan setting on this firmware — fans stay under Braiins' control."; fi
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
$SSH "cat > /etc/init.d/s19volt" <<I
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
# ask the firmware to keep our files across Braiins upgrades (standard OpenWrt keep list)
$SSH 'chmod 755 /etc/init.d/s19volt; mkdir -p /lib/upgrade/keep.d; printf "/etc/s19volt/\n/etc/init.d/s19volt\n" > /lib/upgrade/keep.d/s19volt
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
(floor check + efficiency search: docs/PSU-TRIM-MOD.md). Fan rule and temperature cap run in
shadow mode (they log what they would do).
After a Braiins firmware upgrade, check the UI still answers; if not, run this installer again
(your core.json level table is kept).
D
