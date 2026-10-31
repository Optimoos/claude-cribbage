#!/bin/sh
# Exports release builds into build/linux and build/windows.
# Needs the Godot 4.7.2 .NET export templates installed, and
# addons/webrtc_native (fetch-webrtc.sh). GODOT names the editor binary.
set -eu
cd "$(dirname "$0")"
GODOT=${GODOT:-../Godot_v4.7.2-stable_mono_linux_x86_64/Godot_v4.7.2-stable_mono_linux.x86_64}
[ -d addons/webrtc_native ] || ./fetch-webrtc.sh
mkdir -p build
touch build/.gdignore # keep exports out of the project and each other
"$GODOT" --headless --path . --import >/dev/null 2>&1 || true
for preset in Linux Windows; do
  dir=build/$(echo "$preset" | tr 'A-Z' 'a-z')
  rm -rf "$dir"
  mkdir -p "$dir"
  out=$dir/Cribbage.x86_64
  [ "$preset" = Windows ] && out=$dir/Cribbage.exe
  "$GODOT" --headless --path . --export-release "$preset" "$out"
done
ls -l build/*
