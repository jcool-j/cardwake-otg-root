#!/data/data/com.termux/files/usr/bin/bash
# Optional helper: installs the card tools and runs the (demo-mode) Go server.
# The Cardwake APK does not need this - it uses its built-in su/dd bridge.
set -e
echo "=== Cardwake Termux helper ==="
pkg update -y && pkg install -y tsu dosfstools exfatprogs e2fsprogs parted util-linux
if [ ! -x ./cardwake-arm64 ]; then
  echo "cardwake-arm64 not found. Build it on a PC: sh go-arm64/build-android-arm64.sh, then copy it here."
  exit 1
fi
echo "Starting on http://127.0.0.1 (see the printed port). Non-Windows builds run in DEMO mode."
tsu -c "./cardwake-arm64"
