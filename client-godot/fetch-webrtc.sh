#!/bin/sh
# Downloads the webrtc-native GDExtension into addons/webrtc_native.
# Desktop Godot has no built-in WebRTC; this provides it.
set -eu
VERSION=1.2.2-stable
SHA256=98e9446921740d995bd9ca1be48798dc3c2ceed51e044a25ce18b3cff11f56e5
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/webrtc.zip" \
  "https://github.com/godotengine/webrtc-native/releases/download/$VERSION/godot-extension-webrtc_native.zip"
echo "$SHA256  $tmp/webrtc.zip" | sha256sum -c -
rm -rf addons/webrtc_native
unzip -q "$tmp/webrtc.zip" 'addons/webrtc_native/*' -d .
echo "webrtc-native $VERSION installed in addons/webrtc_native"
