#!/bin/sh
# Cross-compile the Cardwake server for Android/Linux arm64 (pure Go, no CGO).
# NOTE: on non-Windows platforms the server runs in DEMO mode (it does not list
# real block devices). The APK itself does NOT use this binary - it talks to the
# card through the native su/dd bridge in MainActivity.kt.
cd "$(dirname "$0")" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o cardwake-arm64 .
