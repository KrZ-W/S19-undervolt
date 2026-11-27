#!/bin/sh
# bos-settings.sh — runs ON the miner (copied by install.sh). Checks the Braiins settings s19volt depends on,
# reports them, and sets the ones that are wrong (backups first; bosminer is stopped only if something changes,
# because it re-saves its config from memory and would overwrite an edit made while it runs).
#   usage: sh bos-settings.sh [check]     (check = report only, change nothing)
T=/etc/bosminer.toml
B=/etc/s19volt
mode=${1:-set}

# toml_set FILE SECTION KEY VALUE: set KEY = VALUE inside [SECTION] ("" = top level, before any section);
# adds the key, or the section, when missing. Other lines are untouched.
toml_set() {
	awk -v sec="$2" -v key="$3" -v val="$4" '
		function flush() { if (insec && !done) { print key " = " val; done = 1 } }
		BEGIN { insec = (sec == ""); done = 0 }
		/^\[/ { flush(); insec = ($0 == "[" sec "]"); if (insec) seen = 1 }
		insec && $0 ~ "^" key " *=" { if (!done) print key " = " val; done = 1; next }
		{ print }
		END { flush(); if (!done && !seen && sec != "") { print ""; print "[" sec "]"; print key " = " val } }
	' "$1"
}
# toml_get FILE SECTION KEY: the raw value, "" if missing
toml_get() {
	awk -v sec="$2" -v key="$3" '
		BEGIN { insec = (sec == "") }
		/^\[/ { insec = ($0 == "[" sec "]") }
		insec && $0 ~ "^" key " *=" { sub("^" key " *= *", ""); print; exit }
	' "$1" 2>/dev/null
}

# want: FILE|SECTION|KEY|VALUE|why
WANT='T|performance_scaling|enabled|false|Dynamic Performance Scaling would change the power target by itself
T|autotuning|enabled|true|the wizard and the level table need Braiins tuner
T|fan_control|speed|15|bosminer starts the fans at this speed after every restart (was 25 %: an audible step up)'

build() { # writes the corrected copies to /tmp; prints what differs
	cp "$T" /tmp/bt.$$ || return 1
	echo "$WANT" | while IFS='|' read f s k v why; do
		file=/tmp/bt.$$
		cur=$(toml_get "$file" "$s" "$k")
		if [ "$cur" != "$v" ]; then
			echo "   ${s:+[$s] }$k: ${cur:-unset} -> $v ($why)"
			toml_set "$file" "$s" "$k" "$v" > "$file.n" && mv "$file.n" "$file"
		else
			echo "   ${s:+[$s] }$k = $v ok"
		fi
	done
}

echo "== Braiins settings"
build
changed=0
cmp -s "$T" /tmp/bt.$$ || changed=1

# temperatures: reported, never changed (they may be deliberate)
hot=$(toml_get "$T" temp_control hot_temp); dang=$(toml_get "$T" temp_control dangerous_temp)
awk -v h="${hot:-0}" -v d="${dang:-0}" 'BEGIN {
	if (h + 0 > 90) printf "   WARNING: [temp_control] hot_temp = %s (above 90 °C)\n", h
	if (d + 0 > 100) printf "   WARNING: [temp_control] dangerous_temp = %s (above 100 °C: Braiins will not shut down on overheat in time)\n", d }'

if [ "$changed" = 0 ]; then echo "   nothing to change"; rm -f /tmp/bt.$$; exit 0; fi
if [ "$mode" = check ]; then echo "   (check only: nothing changed)"; rm -f /tmp/bt.$$; exit 0; fi

mkdir -p "$B"
[ -f "$B/bosminer.toml.pre-install" ] || cp -p "$T" "$B/bosminer.toml.pre-install"
echo "   applying (bosminer restarts; originals in $B/*.pre-install)"
/etc/init.d/bosminer stop >/dev/null 2>&1; sleep 3
build >/dev/null          # again from the files bosminer just saved
cp /tmp/bt.$$ "$T"
rm -f /tmp/bt.$$
/etc/init.d/bosminer start >/dev/null 2>&1
echo "   done"
