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
  [ -d /etc/s19volt ] && mv /etc/s19volt /etc/s19volt.removed-$(date +%Y%m%d-%H%M%S)
  echo "removed; Braiins OS+ untouched."'
