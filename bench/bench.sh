#!/usr/bin/env bash
# Times a few simple workloads in a directory. Run it once on a tuplefs mount
# and once on a local directory to compare:
#
#   bench/bench.sh /mnt/tuplefs
#   bench/bench.sh /tmp/localdir
#
# The whole set runs RUNS times and the median of each workload is reported,
# because single runs vary a lot, most of all on shared machines. Small-file
# counts and the large-file size can be changed with FILES and MB.
set -euo pipefail
dir=${1:?usage: bench.sh <directory>}
RUNS=${RUNS:-3}
FILES=${FILES:-5000}
MB=${MB:-256}
results=$(mktemp)
work=
trap 'rm -rf "$results" ${work:+"$work"}' EXIT

now() { date +%s.%N; }
record() { # label, start, end, count, unit
	awk -v l="$1" -v s="$2" -v e="$3" -v n="$4" -v u="$5" \
		'BEGIN { t = e - s; printf "%s\t%.3f\t%.1f\t%s\n", l, t, n / t, u }' >>"$results"
}

payload=$(head -c 4096 /dev/zero | tr '\0' x)
for ((run = 1; run <= RUNS; run++)); do
	work="$dir/bench.$$.$run"
	mkdir "$work"

	s=$(now)
	dd if=/dev/zero of="$work/big" bs=1M count="$MB" conv=fsync status=none
	e=$(now); record "write ${MB}MiB sequential + fsync" "$s" "$e" "$MB" "MiB/s"

	s=$(now)
	dd if="$work/big" of=/dev/null bs=1M status=none
	e=$(now); record "read ${MB}MiB sequential" "$s" "$e" "$MB" "MiB/s"

	s=$(now)
	dd if=/dev/zero of="$work/big" bs=4k count=2000 seek=1000 conv=notrunc,fsync status=none
	e=$(now); record "overwrite 2000 x 4KiB + fsync" "$s" "$e" 2000 "writes/s"
	rm "$work/big"

	mkdir "$work/small"
	s=$(now)
	for ((i = 0; i < FILES; i++)); do printf %s "$payload" >"$work/small/f$i"; done
	e=$(now); record "create $FILES files of 4KiB" "$s" "$e" "$FILES" "files/s"

	s=$(now)
	ls -l "$work/small" >/dev/null
	e=$(now); record "ls -l of $FILES files" "$s" "$e" "$FILES" "files/s"

	s=$(now)
	cat "$work/small"/* >/dev/null
	e=$(now); record "read $FILES files" "$s" "$e" "$FILES" "files/s"

	s=$(now)
	rm -r "$work/small"
	e=$(now); record "delete $FILES files" "$s" "$e" "$FILES" "files/s"

	rm -rf "$work"
	work=
done

# One line per workload, in the order they ran: the median rate, then every
# run so the spread is visible.
awk -F'\t' -v runs="$RUNS" '
	!($1 in n) { order[++labels] = $1 }
	{ n[$1]++; rate[$1, n[$1]] = $3; unit[$1] = $4 }
	END {
		for (k = 1; k <= labels; k++) {
			l = order[k]; c = n[l]
			for (i = 1; i <= c; i++) v[i] = rate[l, i] + 0
			for (i = 1; i <= c; i++) for (j = i + 1; j <= c; j++) if (v[j] < v[i]) { t = v[i]; v[i] = v[j]; v[j] = t }
			med = (c % 2) ? v[(c + 1) / 2] : (v[c / 2] + v[c / 2 + 1]) / 2
			all = ""
			for (i = 1; i <= c; i++) all = all sprintf("%s%.0f", i > 1 ? " " : "", v[i])
			printf "%-32s %9.0f %-9s (runs: %s)\n", l, med, unit[l], all
		}
	}' "$results"
