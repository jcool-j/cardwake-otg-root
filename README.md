# Cardwake OTG & Root

אפליקציית אנדרואיד אופליין (עברית) לעבודה עם כרטיסי SD וכונני USB: שחזור קבצים, אבחון, בדיקת זיוף נפח, מפת סקטורים, פירמוט FAT32 וכספת מוצפנת. הממשק הוא PWA מובנה שרץ ב-WebView, עם גשר Root (`su`) לקריאה/כתיבה של התקני בלוק, ותמיכת OTG דרך USB host API.

## מבנה

- `app/` - אפליקציית האנדרואיד (Kotlin, AGP 8.5.2, compileSdk 34, minSdk 26)
- `app/src/main/assets/www/` - ה-PWA המובנה (בילד מוכן, עובד אופליין דרך WebViewAssetLoader)
- `app/src/main/assets/www/assets/aistudio/` - קוד המקור של שרת ה-Go (Cardwake Root Bridge)
- `go-arm64/` - סקריפט בנייה של שרת ה-Go ל-linux/arm64 (Termux)
- `termux/` - סקריפט התקנה ל-Termux עם root

## בניית APK

דרוש JDK 17 ו-Android SDK (platform 34):

```bash
./gradlew assembleDebug
# app/build/outputs/apk/debug/app-debug.apk
```

## בניית שרת ה-Go (linux/arm64)

השרת דורש `go.mod` (go 1.24+) ותיקיית `ui/` עם `index.html` ליד קבצי ה-Go:

```bash
cd go-arm64
./build-android-arm64.sh   # GOOS=linux GOARCH=arm64
```
