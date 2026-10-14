#!/usr/bin/env bash
# Builds the release zips into dist/: Linux and Windows in Docker, Darwin on a
# Mac over SSH, both from the same snapshot of the tree. Each library is loaded
# with tools/loadcheck where it can run here or on the Mac, darwin/amd64 under
# Rosetta; linux/arm64 and windows/amd64 are loaded by
# .github/workflows/verify-release.yml from the draft release instead, which
# also loads darwin/amd64 on a native Intel Mac.
#
# darwin/amd64 is built with go-tls-slot.patch applied to the toolchain. Stock
# Go keeps g in TLS slot 6 (0x30(GS)) in every runtime there, so a Go plugin
# and the Go host overwrite each other's and CPA crashes on the first plugin
# call that grows a stack. The patched runtime uses slot 11 (0x58(GS)).
#
#   MAC_HOST     ssh destination of the Mac (default: macbook)
#   MAC_PROXY    http proxy for downloads on the Mac (optional)
#   DIST_DIRTY=1 builds an uncommitted tree; a release never should
#
# Runs on bash 3.2, which is what the Mac has.
set -euo pipefail

plugin=cpa-codex-weight-balancer
go_toolchain=go1.26.2
go_src_sha256=2e91ebb6947a96e9436fb2b3926a8802efe63a6d375dffec4f82aa9dbd6fd43b
# Go 1.26's own floor. Left unset, the Mac's SDK stamps its own version and
# the library refuses to load on anything older.
macos_min=12.0

die() {
	echo "dist: $*" >&2
	exit 1
}

cd "$(dirname "$0")/.."
version=$(sed -n 's/^VERSION *:= *//p' Makefile)
grep -q "pluginVersion = \"$version\"" plugin/runtime.go || die "Makefile VERSION $version does not match pluginVersion in plugin/runtime.go"

# build GOOS GOARCH CC EXT
build() {
	local out="out/$1_$2"
	mkdir -p "$out" logs
	GOOS=$1 GOARCH=$2 CC=$3 go build -trimpath -ldflags="-s -w" -buildmode=c-shared \
		-o "$out/$plugin.$4" ./plugin 2>"logs/$1_$2.log" || { cat "logs/$1_$2.log" >&2; die "build $1/$2 failed"; }
	rm "$out/$plugin.h"
}

check_linux() {
	local max
	max=$(readelf --dyn-syms -W "$1" | grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -1)
	[ "$(printf '%s\n2.17\n' "$max" | sort -V | tail -1)" = 2.17 ] || die "$1 needs glibc $max; CPA's Linux build runs on 2.17"
	readelf --dyn-syms -W "$1" | awk '$8 == "cliproxy_plugin_init" && $7 != "UND" { found = 1 } END { exit !found }' ||
		die "$1 does not export cliproxy_plugin_init"
}

check_windows() {
	local imports
	imports=$(x86_64-w64-mingw32-objdump -p "$1" | sed -n 's/^\tDLL Name: //p' | LC_ALL=C sort -u | tr '\n' ' ')
	# Anything beyond these would have to ship next to the DLL.
	[ "$imports" = "KERNEL32.dll msvcrt.dll " ] || die "$1 imports $imports"
	x86_64-w64-mingw32-objdump -p "$1" | grep -w cliproxy_plugin_init >/dev/null || die "$1 does not export cliproxy_plugin_init"
}

check_darwin() {
	! grep -q 'built for newer' "$2" || die "$1 links objects built for a newer macOS than $macos_min"
	[ "$(vtool -show-build "$1" | awk '/minos/ { print $2 }')" = "$macos_min" ] || die "$1 does not target macOS $macos_min"
	nm -gU "$1" | grep ' T _cliproxy_plugin_init$' >/dev/null || die "$1 does not export cliproxy_plugin_init"
}

check_slot() {
	otool -tv "$1" | awk '/%gs:0x30/ { stock++ } /%gs:0x58/ { patched++ } END { exit !(patched > 0 && !stock) }' ||
		die "$1 keeps g outside TLS slot 11"
}

# Builds $go_toolchain with go-tls-slot.patch into $tls_root/go, once per
# toolchain and patch. Sets a global: bash 3.2 does not stop on errors inside
# a command substitution.
tls_toolchain() {
	local stamp bootstrap
	tls_root=$HOME/Library/Caches/cpa-wb-go-tls
	stamp="$go_toolchain $(shasum -a 256 scripts/go-tls-slot.patch | cut -d' ' -f1)"
	[ "$(cat "$tls_root/go/.stamp" 2>/dev/null)" != "$stamp" ] || return 0
	# The stock $go_toolchain selected by GOTOOLCHAIN bootstraps its own source.
	bootstrap=${GOROOT_BOOTSTRAP:-$(go env GOROOT)}
	[ -x "$bootstrap/bin/go" ] || die "no Go at '$bootstrap' to bootstrap the patched toolchain"
	rm -rf "$tls_root"
	mkdir -p "$tls_root"
	curl -fsSL -o "$tls_root/src.tar.gz" "https://go.dev/dl/$go_toolchain.src.tar.gz"
	echo "$go_src_sha256  $tls_root/src.tar.gz" | shasum -a 256 -c - >/dev/null || die "$go_toolchain source checksum mismatch"
	tar -xzf "$tls_root/src.tar.gz" -C "$tls_root"
	rm "$tls_root/src.tar.gz"
	patch -d "$tls_root/go" -p1 --forward <scripts/go-tls-slot.patch >/dev/null || die "go-tls-slot.patch does not apply to $go_toolchain"
	(cd "$tls_root/go/src" && env -u GOTOOLCHAIN -u GOFLAGS -u CGO_CFLAGS -u CGO_LDFLAGS -u MACOSX_DEPLOYMENT_TARGET \
		GOCACHE="$tls_root/cache" GOROOT_BOOTSTRAP="$bootstrap" ./make.bash >"$tls_root/make.log" 2>&1) ||
		{ cat "$tls_root/make.log" >&2; die "building the patched $go_toolchain failed"; }
	echo "$stamp" >"$tls_root/go/.stamp"
}

# Inside the image built from scripts/Dockerfile.
cross() {
	export CGO_ENABLED=1 GOFLAGS=-buildvcs=false
	build linux amd64 "zig cc -target x86_64-linux-gnu.2.17" so
	build linux arm64 "zig cc -target aarch64-linux-gnu.2.17" so
	build windows amd64 x86_64-w64-mingw32-gcc dll
	check_linux "out/linux_amd64/$plugin.so"
	check_linux "out/linux_arm64/$plugin.so"
	check_windows "out/windows_amd64/$plugin.dll"
	go run ./tools/loadcheck -version "$version" "out/linux_amd64/$plugin.so"
}

# A library that hangs its host's exit would hang the release with it. macOS
# has no timeout(1), and Go ignores SIGALRM.
bounded_loadcheck() {
	local pid n=0 status=0
	"$@" &
	pid=$!
	while kill -0 "$pid" 2>/dev/null; do
		[ "$n" -lt 120 ] || { kill -9 "$pid" 2>/dev/null || true; break; }
		sleep 1
		n=$((n + 1))
	done
	wait "$pid" || status=$?
	[ "$status" -eq 0 ] || die "loadcheck exited with status $status: $*"
}

# On an Apple Silicon Mac, which can load what it builds, amd64 under Rosetta.
darwin() {
	[ "$(uname -m)" = arm64 ] || die "darwin is built and loaded on an Apple Silicon Mac"
	export PATH="/usr/local/go/bin:/opt/homebrew/bin:$PATH" GOTOOLCHAIN=$go_toolchain GOFLAGS=-buildvcs=false CGO_ENABLED=1
	# A fresh cache: objects cached under another deployment target would be
	# linked as they are.
	export GOCACHE="$PWD/.gocache" MACOSX_DEPLOYMENT_TARGET=$macos_min
	export CGO_CFLAGS="-O2 -g -mmacosx-version-min=$macos_min" CGO_LDFLAGS="-mmacosx-version-min=$macos_min"
	build darwin arm64 clang dylib
	check_darwin "out/darwin_arm64/$plugin.dylib" logs/darwin_arm64.log
	go build -o logs/loadcheck ./tools/loadcheck
	bounded_loadcheck logs/loadcheck -version "$version" "out/darwin_arm64/$plugin.dylib"

	tls_toolchain
	# A cache of its own: a release toolchain's tool IDs are its version string,
	# which the stock and the patched toolchain share. A shared cache has not
	# mixed them up so far, as the patched runtime source keys its own entries,
	# but nothing would key a change to the linker alone.
	(
		export GOROOT="$tls_root/go" GOTOOLCHAIN=local GOCACHE="$tls_root/cache" PATH="$tls_root/go/bin:$PATH"
		build darwin amd64 "clang -arch x86_64" dylib
	)
	check_darwin "out/darwin_amd64/$plugin.dylib" logs/darwin_amd64.log
	check_slot "out/darwin_amd64/$plugin.dylib"
	# The host side stays stock, as in CPA.
	GOARCH=amd64 CC="clang -arch x86_64" go build -o logs/loadcheck_amd64 ./tools/loadcheck
	bounded_loadcheck arch -x86_64 logs/loadcheck_amd64 -version "$version" "out/darwin_amd64/$plugin.dylib"
}

# Globals, so the EXIT trap still sees them after release returns.
mac=${MAC_HOST:-macbook}
work=""
remote=""

# Remote commands go through the Mac's shell; every value spliced in is quoted.
quote() {
	printf '%q' "$1"
}

# Runs after failures too, so it must neither stop at its own first error nor
# replace the status it was called with.
cleanup() {
	local status=$?
	trap - EXIT
	set +e
	[ -z "$work" ] || rm -rf "$work"
	[ -z "$remote" ] || ssh "$mac" "rm -rf $(quote "$remote")" || echo "dist: could not remove $mac:$remote" >&2
	exit "$status"
}

release() {
	[ -n "${DIST_DIRTY:-}" ] || [ -z "$(git status --porcelain)" ] || die "working tree is dirty; DIST_DIRTY=1 builds it anyway"
	local image dir lib n proxy=""
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	work=$(mktemp -d)
	git ls-files -z -c -o --exclude-standard | tar --null -T - -cf "$work/src.tar"

	# The image's zig is x86_64, so the image must be too, whatever this host is.
	image=cpa-wb-build:$(sha256sum scripts/Dockerfile | cut -c1-12)
	[ "$(docker image inspect -f '{{.Architecture}}' "$image" 2>/dev/null)" = amd64 ] ||
		docker build --platform linux/amd64 -q -t "$image" scripts >/dev/null
	docker run --rm --platform linux/amd64 -v "$work:/work" -v cpa-wb-gomod:/go/pkg/mod -v cpa-wb-cache:/root/.cache "$image" \
		bash -euo pipefail -c "mkdir /src && tar -xf /work/src.tar -C /src && cd /src && bash scripts/dist.sh cross && cp -r out/. /work/ && chown -R $(id -u):$(id -g) /work"

	remote=$(ssh "$mac" mktemp -d)
	ssh "$mac" "tar -xf - -C $(quote "$remote")" <"$work/src.tar"
	[ -z "${MAC_PROXY:-}" ] || proxy="env https_proxy=$(quote "$MAC_PROXY") http_proxy=$(quote "$MAC_PROXY") "
	ssh "$mac" "cd $(quote "$remote") && ${proxy}bash scripts/dist.sh darwin"
	# bsdtar would otherwise add ._* AppleDouble files and xattr records.
	ssh "$mac" "COPYFILE_DISABLE=1 tar --no-mac-metadata --no-xattrs -cf - -C $(quote "$remote/out") ." | tar -xf - -C "$work"

	# Packaged aside first: a failure from here on must not leave dist/ with
	# half a release in it.
	mkdir -p "$work/release"
	for dir in "$work"/*_*/; do
		lib=$(ls "$dir")
		[ "$lib" = "$plugin.so" ] || [ "$lib" = "$plugin.dll" ] || [ "$lib" = "$plugin.dylib" ] || die "unexpected files in $dir: $lib"
		zip -q -j -X "$work/release/${plugin}_${version}_$(basename "$dir").zip" "$dir$lib"
	done
	n=$(find "$work/release" -name '*.zip' | wc -l)
	[ "$n" -eq 5 ] || die "built $n platforms, expected 5"
	(cd "$work/release" && sha256sum -- *.zip >checksums.txt)

	mkdir -p dist
	rm -f dist/*.zip dist/checksums.txt
	mv "$work/release"/* dist/ || {
		rm -f dist/*.zip dist/checksums.txt
		die "could not move the release into dist/"
	}
	cat dist/checksums.txt
}

case "${1:-}" in
"") release ;;
cross) cross ;;
darwin) darwin ;;
*) die "unknown mode $1" ;;
esac
