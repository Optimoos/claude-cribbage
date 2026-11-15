#!/bin/sh
# Exports release builds into build/linux and build/windows, and zips each
# with a README and licences: build/Cribbage-<version>-<os>-x86_64.zip.
# Needs the Godot 4.7.2 .NET export templates installed (ci/install-godot.sh
# installs them, with the editor), and addons/webrtc_native
# (fetch-webrtc.sh). GODOT names the editor binary.
#
# CRIBBAGE_SERVER, the game server's https:// address as the web page
# uses it (e.g. https://api.cribbage.example.com), makes wss://<host>/rtc
# the builds' default server; without it they default to a local server.
# CRIBBAGE_VERSION names the version (default: dev).
set -eu
cd "$(dirname "$0")"
GODOT=${GODOT:-../Godot_v4.7.2-stable_mono_linux_x86_64/Godot_v4.7.2-stable_mono_linux.x86_64}
export CRIBBAGE_VERSION=${CRIBBAGE_VERSION:-dev}
case $CRIBBAGE_VERSION in
"" | *[!A-Za-z0-9.+-]*)
	echo "export.sh: CRIBBAGE_VERSION may hold letters, digits, '.', '+' and '-'" >&2
	exit 1
	;;
esac
server=ws://127.0.0.1:8080/rtc
if [ -n "${CRIBBAGE_SERVER:-}" ]; then
	host=${CRIBBAGE_SERVER%/}
	host=${host#https://}
	case $CRIBBAGE_SERVER in
	https://*) ;;
	*)
		echo "export.sh: CRIBBAGE_SERVER must be the server's https:// address" >&2
		exit 1
		;;
	esac
	case $host in
	"" | */* | *[!A-Za-z0-9.:-]*)
		echo "export.sh: CRIBBAGE_SERVER must be https://<host name>, with no path" >&2
		exit 1
		;;
	esac
	server=wss://$host/rtc
	export CRIBBAGE_DEFAULT_SERVER=$server # read by Cribbage.csproj
fi

[ -d addons/webrtc_native ] || ./fetch-webrtc.sh
mkdir -p build
touch build/.gdignore # keep exports out of the project and each other
"$GODOT" --headless --path . --import >/dev/null 2>&1 || true
for preset in Linux Windows; do
	os=$(echo "$preset" | tr 'A-Z' 'a-z')
	dir=build/$os
	rm -rf "$dir"
	mkdir -p "$dir"
	out=$dir/Cribbage.x86_64
	[ "$preset" = Windows ] && out=$dir/Cribbage.exe
	"$GODOT" --headless --path . --export-release "$preset" "$out"
	[ -s "$out" ] || { echo "export.sh: $preset export failed" >&2; exit 1; }

	# The zip: one Cribbage folder, with a README and the licences.
	stage=build/stage/Cribbage
	rm -rf build/stage
	mkdir -p "$stage/licenses"
	cp -R "$dir/." "$stage/"
	cp THIRD-PARTY-NOTICES.txt addons/webrtc_native/LICENSE.* "$stage/licenses/"
	sed -e "s|@VERSION@|$CRIBBAGE_VERSION|" -e "s|@SERVER@|$server|" ci/README.txt >"$stage/README.txt"
	zip=build/Cribbage-$CRIBBAGE_VERSION-$os-x86_64.zip
	rm -f "$zip"
	(cd build/stage && zip -qr9 "../$(basename "$zip")" Cribbage)
	rm -rf build/stage
done
ls -l build/*.zip
