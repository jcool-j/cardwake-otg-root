#!/usr/bin/env bash
set -e
echo "Building Cardwake OTG & Root Debug APK..."
gradle wrapper --gradle-version 8.7 || true
./gradlew assembleDebug
echo "APK ready at: app/build/outputs/apk/debug/app-debug.apk"
