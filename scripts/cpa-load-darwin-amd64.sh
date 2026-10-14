#!/usr/bin/env bash
# Loads a darwin/amd64 library into the official CPA darwin/amd64 release and
# fails unless CPA registers it, stays up through a soak of status and
# refresh calls with GOGC=10, idles without burning CPU once the plugin is
# disabled, serves it again once re-enabled, and then stops cleanly. Two
# runtimes sharing a g slot corrupt the heap within seconds; refresh calls
# back into the host, so both directions of the crossing run. Disabling
# dlcloses the library, and an image dyld unmaps under the plugin's running Go
# runtime leaves its threads spinning. On an Apple Silicon Mac CPA runs under
# Rosetta.
#
# CPA gets an empty auth-dir and the plugin dry_run, and the proxy points at a
# closed loopback port: nothing leaves the machine but the CPA download, which
# alone honours https_proxy.
#
#   SOAK_SECONDS  default 120
#
# Usage: cpa-load-darwin-amd64.sh <library>
set -euo pipefail

cpa_version=8.0.27
cpa_sha256=8ca6317fe7560a0452d2835f296a6360de8bc88099670c68635fd1cf4132c8d5
plugin=cpa-codex-weight-balancer
soak=${SOAK_SECONDS:-120}

lib=${1:?usage: cpa-load-darwin-amd64.sh <library>}
# Decimal without leading zeros, which bash would read as octal, and short
# enough not to overflow.
case $soak in
'' | *[!0-9]* | 0* | ??????*)
	echo "cpa-load: SOAK_SECONDS must be a whole number from 1 to 99999" >&2
	exit 2
	;;
esac
work=$(mktemp -d)
log=$work/cpa.log
pid=""

fail() {
	echo "cpa-load: $*" >&2
	tail -n 100 "$log" >&2 || true
	exit 1
}

# Stops CPA when a failure left it running, within 15s so that a hang cannot
# hold the job.
cleanup() {
	local status=$?
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		for _ in $(seq 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
		kill -9 "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

asset=CLIProxyAPI_${cpa_version}_darwin_amd64.tar.gz
curl -fsSL -o "$work/$asset" "https://github.com/router-for-me/CLIProxyAPI/releases/download/v$cpa_version/$asset"
echo "$cpa_sha256  $work/$asset" | shasum -a 256 -c - >/dev/null || fail "$asset checksum mismatch"
mkdir -p "$work/cpa" "$work/auth" "$work/home" "$work/plugins"
tar -xzf "$work/$asset" -C "$work/cpa"
cp "$lib" "$work/plugins/$plugin.dylib"

key=$(openssl rand -hex 24)
port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
cat >"$work/config.yaml" <<EOF
host: "127.0.0.1"
port: $port
auth-dir: "$work/auth"
logging-to-file: false
remote-management:
  allow-remote: false
  secret-key: "$key"
  disable-control-panel: true
routing:
  strategy: "weighted-round-robin"
plugins:
  enabled: true
  dir: "$work/plugins"
  configs:
    $plugin:
      enabled: true
      dry_run: true
EOF

launch=()
[ "$(uname -m)" != arm64 ] || launch=(arch -x86_64)
dead=http://127.0.0.1:9
# From the temp dir: CPA reads .env from its working directory.
(cd "$work" && exec env HOME="$work/home" GOGC=10 HTTP_PROXY=$dead HTTPS_PROXY=$dead ALL_PROXY=$dead NO_PROXY=127.0.0.1,localhost \
	${launch[@]+"${launch[@]}"} "$work/cpa/cli-proxy-api" -config "$work/config.yaml") >"$log" 2>&1 &
pid=$!

mgmt=http://127.0.0.1:$port/v0/management
api=$mgmt/$plugin
# Prints the HTTP status, and fails when the exchange does, even after a 200.
code() {
	curl -s --noproxy '*' -o /dev/null -w '%{http_code}' --max-time 10 -H "Authorization: Bearer $key" "$@"
}
check_log() {
	local status=0
	grep -E 'fatal|panic|SIGSEGV|SIGBUS|unexpected return pc|failed to load plugin' "$log" || status=$?
	case $status in
	1) ;;
	0) fail "CPA log reports a failure" ;;
	*) fail "could not read the CPA log" ;;
	esac
}
# CPU seconds CPA has used, from ps's colon-separated time; CPA runs here for
# minutes, not days.
cpu_seconds() {
	ps -o time= -p "$pid" | awk -F: '{ s = 0; for (i = 1; i <= NF; i++) s = s * 60 + $i; printf "%d\n", s } END { if (!NR) exit 1 }'
}
await_status() {
	for _ in $(seq 60); do
		kill -0 "$pid" 2>/dev/null || fail "CPA exited before the status route answered"
		if got=$(code "$api/status") && [ "$got" = 200 ]; then
			return
		fi
		sleep 1
	done
	fail "status route never answered 200"
}
# soak_for SECONDS
soak_for() {
	local start=$calls end=$((SECONDS + $1))
	while [ "$SECONDS" -lt "$end" ]; do
		kill -0 "$pid" 2>/dev/null || fail "CPA exited after $calls rounds"
		got=$(code "$api/status") || fail "status request failed after $calls rounds"
		[ "$got" = 200 ] || fail "status answered $got after $calls rounds"
		got=$(code -X POST "$api/refresh") || fail "refresh request failed after $calls rounds"
		[ "$got" = 200 ] || fail "refresh answered $got after $calls rounds"
		calls=$((calls + 1))
	done
	kill -0 "$pid" 2>/dev/null || fail "CPA exited at the end of a soak"
	[ "$calls" -gt "$start" ] || fail "no status+refresh round completed"
	check_log
}
# set_enabled true|false
set_enabled() {
	got=$(code -X PATCH -H 'Content-Type: application/json' -d "{\"enabled\":$1}" "$mgmt/plugins/$plugin/enabled") ||
		fail "setting enabled to $1 failed"
	[ "$got" = 200 ] || fail "setting enabled to $1 answered $got"
}

await_status
check_log
calls=0
soak_for "$soak"

set_enabled false
unloaded=""
for _ in $(seq 30); do
	if grep -q 'pluginhost: plugin unloaded' "$log"; then
		unloaded=1
		break
	fi
	sleep 1
done
[ -n "$unloaded" ] || fail "CPA did not unload the plugin within 30s of disabling it"
idle=60
before=$(cpu_seconds) || fail "could not read CPA's CPU time"
sleep "$idle"
after=$(cpu_seconds) || fail "CPA exited after unloading the plugin"
used=$((after - before))
[ "$used" -ge 0 ] && [ "$used" -lt $((idle / 2)) ] || fail "CPA used ${used}s of CPU in the ${idle}s after unloading the plugin"
check_log

set_enabled true
await_status
soak_for 30

kill "$pid" || true
for _ in $(seq 30); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
! kill -0 "$pid" 2>/dev/null || fail "CPA still running 30s after SIGTERM"
status=0
wait "$pid" || status=$?
pid=""
[ "$status" -eq 0 ] || fail "CPA exited with status $status on SIGTERM"
check_log
echo "cpa-load: ok, CPA $cpa_version alive after $calls status+refresh rounds with GOGC=10, idle at ${used}s of CPU over ${idle}s with the plugin unloaded, then stopped cleanly"
