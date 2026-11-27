#!/bin/sh
# Remove the s19volt add-on and put the miner back to plain Braiins OS+.
#   ./uninstall.sh <miner-ip>      (keeps a copy of your config in /etc/s19volt.removed-<date>)
set -e
M=${1:?usage: ./uninstall.sh <miner-ip>}
ssh -o StrictHostKeyChecking=accept-new "root@$M" '
  /etc/init.d/s19volt stop 2>/dev/null || true   # s19volt leaves the V2 pin LOW (stock voltage) on stop
  /etc/s19volt/ch3test low 2>/dev/null || true
  /etc/init.d/s19volt disable 2>/dev/null || true
  rm -f /etc/init.d/s19volt /lib/upgrade/keep.d/s19volt
  /etc/init.d/s19volt-bospatch disable 2>/dev/null || true; rm -f /etc/init.d/s19volt-bospatch
  if grep -q " /usr/bin/bosminer " /proc/mounts; then   # back to the stock bosminer now (it restarts)
    /etc/init.d/bosminer stop; i=0; while pidof bosminer >/dev/null && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done; umount /usr/bin/bosminer; rm -f /tmp/bosminer.patched; /etc/init.d/bosminer start
  fi
  uci set bos.auto_upgrade.enable=1 && uci commit bos; /etc/init.d/bos_autoupgrade enable; /etc/init.d/bos_autoupgrade start
  echo "Braiins auto-upgrade back on."
  [ -d /etc/s19volt ] && mv /etc/s19volt /etc/s19volt.removed-$(date +%Y%m%d-%H%M%S)
  echo "removed; Braiins OS+ untouched."'
