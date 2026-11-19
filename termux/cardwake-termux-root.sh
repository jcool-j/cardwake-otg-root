#!/data/data/com.termux/files/usr/bin/bash
set -e
echo "=== Cardwake Android OTG & Root Setup ==="
pkg update -y && pkg install -y nodejs-lts tsu dosfstools exfatprogs e2fsprogs parted util-linux
echo "Starting Cardwake Root Bridge on http://127.0.0.1:3000 ..."
tsu -c "NODE_ENV=production node server.ts"
