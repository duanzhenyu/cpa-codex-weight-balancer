#!/usr/bin/env bash
# Loads a darwin/amd64 library into the official CPA darwin/amd64 release and
# fails unless CPA registers it, stays up through a soak of status and
# refresh calls with GOGC=10, and then stops cleanly, unloading the plugin.
# Two runtimes sharing a g slot corrupt the heap within seconds; refresh calls
# back into the host, so both directions of the crossing run. On an Apple
# Silicon Mac CPA runs under Rosetta.
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

api=http://127.0.0.1:$port/v0/management/$plugin
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

up=""
for _ in $(seq 60); do
	kill -0 "$pid" 2>/dev/null || fail "CPA exited during startup"
	if got=$(code "$api/status") && [ "$got" = 200 ]; then
		up=1
		break
	fi
	sleep 1
done
[ -n "$up" ] || fail "status route never answered 200"
check_log

calls=0
end=$((SECONDS + soak))
while [ "$SECONDS" -lt "$end" ]; do
	kill -0 "$pid" 2>/dev/null || fail "CPA exited after $calls rounds"
	got=$(code "$api/status") || fail "status request failed after $calls rounds"
	[ "$got" = 200 ] || fail "status answered $got after $calls rounds"
	got=$(code -X POST "$api/refresh") || fail "refresh request failed after $calls rounds"
	[ "$got" = 200 ] || fail "refresh answered $got after $calls rounds"
	calls=$((calls + 1))
done
kill -0 "$pid" 2>/dev/null || fail "CPA exited at the end of the soak"
[ "$calls" -gt 0 ] || fail "no status+refresh round completed"
check_log

kill "$pid" || true
for _ in $(seq 30); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
! kill -0 "$pid" 2>/dev/null || fail "CPA still running 30s after SIGTERM"
status=0
wait "$pid" || status=$?
pid=""
[ "$status" -eq 0 ] || fail "CPA exited with status $status on SIGTERM"
check_log
echo "cpa-load: ok, CPA $cpa_version alive after $calls status+refresh rounds over ${soak}s with GOGC=10, then stopped cleanly"
