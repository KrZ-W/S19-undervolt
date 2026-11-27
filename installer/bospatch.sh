#!/bin/sh
# patch25: expose bosminer's hidden per-chip metrics on Braiins OS+ 25.01 (bosminer md5 10ed4f2707b9ad44f043e52b92ed7b54).
# Copies /rom/usr/bin/bosminer to RAM, rewrites 4 words (add r0,sp,#0x60 -> mov r0,fp: register into the served
# registry), bind-mounts the copy over /usr/bin/bosminer. Nothing on flash changes; reboot/unmount = stock.
set -e
SRC=/rom/usr/bin/bosminer; DST=/tmp/bosminer.patched
[ "$(md5sum $SRC | cut -d' ' -f1)" = 10ed4f2707b9ad44f043e52b92ed7b54 ] || { echo "not the pinned 25.01 bosminer - no patch"; exit 2; }
cp $SRC $DST
# file offset = VA - 0x10000; old word e28d0060 (LE 60 00 8d e2) -> e1a0000b (LE 0b 00 a0 e1)
for off in 0x8ac124 0x8ac218 0x8ac24c 0x8ac27c; do   # hashboard_voltage chip_frequency chip_shares chip_hashrate
  o=$((off))
  cur=$(dd if=$DST bs=1 skip=$o count=4 2>/dev/null | hexdump -v -e '4/1 "%02x"')
  [ "$cur" = 60008de2 ] || { echo "unexpected word $cur at $off"; rm -f $DST; exit 3; }
  printf '\013\000\240\341' | dd of=$DST bs=1 seek=$o count=4 conv=notrunc 2>/dev/null
done
chmod 755 $DST
grep -q " /usr/bin/bosminer " /proc/mounts && umount /usr/bin/bosminer
mount --bind $DST /usr/bin/bosminer
echo "patched: $(md5sum /usr/bin/bosminer | cut -d' ' -f1)"
