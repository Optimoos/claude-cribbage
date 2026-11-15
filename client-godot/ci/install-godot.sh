#!/bin/sh
# Installs the Godot .NET editor and its export templates for export.sh, as
# GitHub Actions does (.github/workflows/desktop-clients.yml). Downloads
# are checked against the hashes in Godot's SHA512-SUMS.txt for this
# release, and kept in $GODOT_CACHE so a cache can skip them next time.
# Prints the editor's path last.
set -eu
VERSION=4.7.2
EDITOR=Godot_v${VERSION}-stable_mono_linux_x86_64
EDITOR_SHA512=1855960b27ee3ef5e66e5e228cced69d55637b24334a7411162687dcd077d8f9f645348cdb8eae984bec8135d49ed855a1e3a16476786b8bce60774fd8402d13
TEMPLATES=Godot_v${VERSION}-stable_mono_export_templates.tpz
TEMPLATES_SHA512=bb5c41d72370ed743660361f6228006f808ab04ca33abdc545d740b044f3fe057f32ae8cb7873a1bc86ddcd82ae683b9f6dfdfe4179852f2c0f1acde2ff6bd5a
URL=https://github.com/godotengine/godot/releases/download/${VERSION}-stable
CACHE=${GODOT_CACHE:-$HOME/.cache/cribbage-godot}

fetch() { # file sha512
	if ! echo "$2  $CACHE/$1" | sha512sum -c --status 2>/dev/null; then
		echo "downloading $1" >&2
		curl -fsSL --retry 3 -o "$CACHE/$1" "$URL/$1"
		echo "$2  $CACHE/$1" | sha512sum -c - >&2
	fi
}

mkdir -p "$CACHE"
fetch "$EDITOR.zip" "$EDITOR_SHA512"
fetch "$TEMPLATES" "$TEMPLATES_SHA512"

# The editor, unpacked beside its zip.
BIN=$CACHE/$EDITOR/Godot_v${VERSION}-stable_mono_linux.x86_64
[ -x "$BIN" ] || unzip -qo "$CACHE/$EDITOR.zip" -d "$CACHE" >&2
# Only the Linux and Windows x86_64 templates (export_presets.cfg).
dest=$HOME/.local/share/godot/export_templates/${VERSION}.stable.mono
mkdir -p "$dest"
unzip -qo -j "$CACHE/$TEMPLATES" 'templates/version.txt' 'templates/linux_*.x86_64' 'templates/windows_*_x86_64*.exe' -d "$dest" >&2
echo "$BIN"
