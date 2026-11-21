#!/usr/bin/env bash
# بناء نزّل الفيديو v2.3 (النسخة القديمة الخفيفة) بدون Gradle:
# aapt2 -> javac -> d8(r8.jar) -> zip -> zipalign -> apksigner
# نفس خط البناء الأصلي الذي أنتج النسخة العاملة 1.33 MB.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-/usr/local/lib/android/sdk}}"
BT="$SDK/build-tools/34.0.0"
PLAT="$SDK/platforms/android-34"
OUT_DIR="$ROOT/out"
OUT="$OUT_DIR/نزّل-الفيديو-2.3.apk"
LIBS="libs/NewPipeExtractor-v0.27.1.jar libs/nanojson.jar libs/jsoup-1.24.1.jar libs/protobuf-javalite-4.36.2.jar libs/rhino-1.8.1.jar libs/rhino-engine-1.8.1.jar libs/jsr305.jar"

cd "$ROOT"
for j in $LIBS; do
  [ -f "$j" ] || { echo "ERROR: مكتبة مفقودة $j"; exit 1; }
done
[ -x "$BT/aapt2" ] || { echo "ERROR: aapt2 غير موجود في $BT"; ls "$SDK/build-tools" 2>/dev/null || true; exit 1; }
[ -f "$PLAT/android.jar" ] || { echo "ERROR: android.jar غير موجود في $PLAT"; ls "$SDK/platforms" 2>/dev/null || true; exit 1; }
[ -x "$ROOT/tools/r8.jar" ] || { echo "ERROR: tools/r8.jar مفقود"; exit 1; }
[ -x "$(command -v javac)" ] || { echo "ERROR: javac غير متوفر"; exit 1; }

rm -rf build gen classes out
mkdir -p build gen classes out

echo "=== 1/7 aapt2 compile+link ==="
"$BT/aapt2" compile --dir res -o build/res.zip
"$BT/aapt2" link -o build/base.apk \
  -I "$PLAT/android.jar" \
  --manifest AndroidManifest.xml \
  -A assets \
  --java gen \
  --min-sdk-version 29 \
  --target-sdk-version 34 \
  --auto-add-overlay \
  build/res.zip

echo "=== 2/7 javac ==="
CP="$PLAT/android.jar:$(echo "$LIBS" | tr ' ' ':')"
javac -source 1.8 -target 1.8 -nowarn -encoding UTF-8 \
  -classpath "$CP" -d classes \
  $(find gen src -name '*.java')

echo "=== 3/7 d8 عبر r8.jar المستقل ==="
java -cp tools/r8.jar com.android.tools.r8.D8 --release --min-api 29 \
  --lib "$PLAT/android.jar" --output build \
  $(find classes -name '*.class') $LIBS

echo "=== 4/7 حزم dex داخل base.apk ==="
cp build/base.apk build/unsigned.apk
(cd build && for f in classes*.dex; do zip -q unsigned.apk "$f"; done)

echo "=== 5/7 zipalign ==="
"$BT/zipalign" -f -p 4 build/unsigned.apk build/aligned.apk

echo "=== 6/7 توقيع (نفس مفتاح debug.keystore الأصلي) ==="
apksigner sign --ks debug.keystore --ks-pass pass:android \
  --key-pass pass:android --ks-key-alias androiddebugkey \
  --v1-signing-enabled true --v2-signing-enabled true --v3-signing-enabled true \
  --out "$OUT" build/aligned.apk

echo "=== 7/7 تحقق ==="
"$BT/apksigner" verify --print-certs "$OUT" | head -3
"$BT/aapt2" dump badging "$OUT" | grep -E "^(package|application-label|sdkVersion|targetSdkVersion|launchable-activity)" || true
ls -la "$OUT"
echo "BUILD OK — نزّل الفيديو v2.3"
