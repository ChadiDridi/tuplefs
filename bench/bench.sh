#!/usr/bin/env bash
# Times a few simple workloads in a directory. Run it once on a tuplefs mount
# and once on a local directory to compare:
#
#   bench/bench.sh /mnt/tuplefs
#   bench/bench.sh /tmp/localdir
#
# Small-file counts and the large-file size can be changed with FILES and MB.
set -euo pipefail
dir=${1:?usage: bench.sh <directory>}
FILES=${FILES:-5000}
MB=${MB:-256}
work="$dir/bench.$$"
mkdir "$work"
trap 'rm -rf "$work"' EXIT

now() { date +%s.%N; }
report() { # label, start, end, count, unit
	awk -v l="$1" -v s="$2" -v e="$3" -v n="$4" -v u="$5" \
		'BEGIN { t = e - s; printf "%-34s %7.2fs  %9.0f %s\n", l, t, n / t, u }'
}

s=$(now)
dd if=/dev/zero of="$work/big" bs=1M count="$MB" conv=fsync status=none
e=$(now); report "write ${MB}MiB sequential + fsync" "$s" "$e" "$MB" "MiB/s"

s=$(now)
dd if="$work/big" of=/dev/null bs=1M status=none
e=$(now); report "read ${MB}MiB sequential" "$s" "$e" "$MB" "MiB/s"

s=$(now)
dd if=/dev/zero of="$work/big" bs=4k count=2000 seek=1000 conv=notrunc,fsync status=none
e=$(now); report "overwrite 2000 x 4KiB + fsync" "$s" "$e" 2000 "writes/s"
rm "$work/big"

payload=$(head -c 4096 /dev/zero | tr '\0' x)
mkdir "$work/small"
s=$(now)
for ((i = 0; i < FILES; i++)); do printf %s "$payload" >"$work/small/f$i"; done
sync "$work/small"
e=$(now); report "create $FILES files of 4KiB" "$s" "$e" "$FILES" "files/s"

s=$(now)
ls -l "$work/small" >/dev/null
e=$(now); report "ls -l of $FILES files" "$s" "$e" "$FILES" "files/s"

s=$(now)
cat "$work/small"/* >/dev/null
e=$(now); report "read $FILES files" "$s" "$e" "$FILES" "files/s"

s=$(now)
for ((i = 0; i < FILES; i++)); do mv "$work/small/f$i" "$work/small/g$i"; done
e=$(now); report "rename $FILES files (one mv each)" "$s" "$e" "$FILES" "files/s"

s=$(now)
rm -r "$work/small"
e=$(now); report "delete $FILES files" "$s" "$e" "$FILES" "files/s"
