#!/usr/bin/env bash
# Compare Rust build times across ways of getting a second working tree:
# a fresh git worktree, an ovnfs overlay on a warm checkout, and a
# copy-on-write clone (APFS on macOS, reflink on Linux).
#
# usage: bench.sh REPO [TOUCH]   run the benchmark
#        bench.sh --clean REPO   remove BENCH_DIR and its worktree
#
#   REPO   git repository of a Cargo project
#   TOUCH  repo-relative file touched for incremental rows (default src/main.rs)
#
# env: BENCH_DIR (default: ovnfs-bench beside REPO, on its volume so clones
#      stay clones), BUILD (default "cargo build"),
#      OVNFS (default: bin/ovnfs next to this script),
#      PROTOS (default "nfs smb" on macOS, "fuse nfs smb" on Linux)
#
# Run BUILD without cross-tree caches (cango, sccache): they would hide the
# filesystem's effect. Linux mounts NFS and SMB only as root, so a non-root
# run mounts them through passwordless sudo. Results go to
# $BENCH_DIR/results.tsv, logs to logs/.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
usage="usage: $0 REPO [TOUCH] | $0 --clean REPO"
clean=
if [[ ${1:-} == --clean ]]; then
	clean=1
	shift
fi
REPO=$(cd "${1:?$usage}" && pwd)
TOUCH=${2:-src/main.rs}
BENCH_DIR=${BENCH_DIR:-$(dirname "$REPO")/ovnfs-bench}
BUILD=${BUILD:-cargo build}
OVNFS=${OVNFS:-$here/bin/ovnfs}
if [[ $(uname -s) == Darwin ]]; then
	PROTOS=${PROTOS:-nfs smb}
	clone_cmd=(cp -c -R)
else
	PROTOS=${PROTOS:-fuse nfs smb}
	clone_cmd=(cp -a --reflink=auto)
fi

if [[ -n $clean ]]; then
	git -C "$REPO" worktree remove --force "$BENCH_DIR/base" 2>/dev/null || true
	rm -rf "$BENCH_DIR"
	exit 0
fi

base=$BENCH_DIR/base clone=$BENCH_DIR/clone
logs=$BENCH_DIR/logs results=$BENCH_DIR/results.tsv

[[ -e $BENCH_DIR ]] && { echo "$BENCH_DIR exists; run: $0 --clean $REPO" >&2; exit 1; }
mkdir -p "$logs"
printf 'row\tseconds\tunits_compiled\n' >"$results"

# Mount and umount for the daemon: sudo-backed where only root may mount.
shim=$BENCH_DIR/sudo-bin
mkdir -p "$shim"
if [[ $(uname -s) == Linux && $(id -u) != 0 ]]; then
	for c in mount umount; do
		printf '#!/bin/sh\nexec sudo -n %s "$@"\n' "$(command -v "$c")" >"$shim/$c"
		chmod +x "$shim/$c"
	done
fi

now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }

# measure LABEL DIR CMD...: time CMD in DIR, count crates cargo compiled.
measure() {
	local label=$1 dir=$2 log t0 t1 secs units
	shift 2
	log=$logs/$(tr -c 'a-zA-Z0-9\n' _ <<<"$label").log
	t0=$(now)
	if ! (cd "$dir" && eval "$@") >"$log" 2>&1; then
		echo "FAILED: $label (log: $log)" >&2
		tail -30 "$log" >&2
		exit 1
	fi
	t1=$(now)
	secs=$(perl -e "printf '%.2f', $t1 - $t0")
	units=$(grep -c '^ *Compiling ' "$log" || true)
	printf '%-44s %9ss %6s compiled\n' "$label" "$secs" "$units"
	printf '%s\t%s\t%s\n' "$label" "$secs" "$units" >>"$results"
}

note() { printf '%-44s %s\n' "$1" "$2" | tee -a "$results"; }

server=
cleanup() {
	if [[ -n $server ]]; then
		if [[ -n ${ov:-} ]]; then
			"$OVNFS" --state "$ov/state" ctl rm '{"checkout":"child"}' >/dev/null 2>&1 || true
			"$OVNFS" --state "$ov/state" ctl rm '{"checkout":"warm"}' >/dev/null 2>&1 || true
		fi
		kill -INT "$server" 2>/dev/null || true
		wait "$server" 2>/dev/null || true
	fi
}
trap cleanup EXIT

echo "repo=$REPO touch=$TOUCH build='$BUILD' dir=$BENCH_DIR"

measure "worktree: git worktree add" "$REPO" git worktree add --quiet --detach "$base" HEAD
measure "worktree: cold build" "$base" "$BUILD"
measure "worktree: no-op build" "$base" "$BUILD"
measure "worktree: incremental (touch)" "$base" "touch $TOUCH && $BUILD"
note "worktree: target size" "$(du -sh "$base/target" | cut -f1)"

# Each protocol reuses base, target/ included, as its read-only lower layer.
for proto in $PROTOS; do
	ov=$BENCH_DIR/overlay-$proto
	proto_log=$logs/ovnfs-$proto.log
	t0=$(now)
	PATH=$shim:$PATH "$OVNFS" serve --proto "$proto" --state "$ov/state" --mount "$ov/tree" >"$proto_log" 2>&1 &
	server=$!
	for _ in $(seq 100); do
		grep -q 'mounted at' "$proto_log" && break
		kill -0 "$server" 2>/dev/null || { cat "$proto_log" >&2; exit 1; }
		sleep 0.1
	done
	note "overlay-$proto: start + mount" "$(perl -e "printf '%.2fs', $(now) - $t0")"
	ctl() { "$OVNFS" --state "$ov/state" ctl "$@"; }
	create_json=$(python3 -c 'import json,sys; print(json.dumps({"name":"warm","dir":sys.argv[1]}))' "$base")
	measure "overlay-$proto: create" "$base" 'ctl new "$create_json"'
	measure "overlay-$proto: first build (lower target warm)" "$ov/tree/warm" "$BUILD"
	measure "overlay-$proto: no-op build" "$ov/tree/warm" "$BUILD"
	checkpoint_json='{"checkout":"warm","label":"warm"}'
	fork_json='{"checkout":"warm","name":"child"}'
	measure "overlay-$proto: checkpoint" "$base" 'ctl snap "$checkpoint_json"'
	measure "overlay-$proto: fork" "$base" 'ctl fork "$fork_json"'
	measure "overlay-$proto: first build after warm fork" "$ov/tree/child" "$BUILD"
	measure "overlay-$proto: incremental (touch)" "$ov/tree/warm" "touch $TOUCH && $BUILD"
	note "overlay-$proto: upper after warm rows" "$(du -sh "$ov/state/uppers" | cut -f1), $(find "$ov/state/uppers" -type f | wc -l | tr -d ' ') files"
	measure "overlay-$proto: cold build, target in upper" "$ov/tree/warm" "CARGO_TARGET_DIR=target-$proto $BUILD"
	note "overlay-$proto: upper after cold row" "$(du -sh "$ov/state/uppers" | cut -f1)"
	cleanup
	server=
done

measure "clone: ${clone_cmd[*]} (incl. target)" "$BENCH_DIR" "${clone_cmd[@]}" "$base" "$clone"
measure "clone: first build" "$clone" "$BUILD"
measure "clone: incremental (touch)" "$clone" "touch $TOUCH && $BUILD"

echo "results: $results"
