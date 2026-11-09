#!/usr/bin/env bash
set -euo pipefail

# Build simulator-safe dependencies without modifying the existing device outputs.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_ROOT="$SCRIPT_DIR/build/morde-simulator"
OUTPUT_ROOT="$BUILD_ROOT/dependencies"
FFMPEG_SOURCE="$SCRIPT_DIR/ffmpeg-6.1.2"
ISH_SOURCE="$SCRIPT_DIR/ish"

for tool in xcrun xcodebuild meson ninja; do
  command -v "$tool" >/dev/null || { echo "error: $tool is required" >&2; exit 1; }
done
for path in "$FFMPEG_SOURCE/configure" "$ISH_SOURCE/meson.build" "$ISH_SOURCE/deps/libapps"; do
  [ -e "$path" ] || { echo "error: required local source is missing: $path" >&2; exit 1; }
done
[ -d "$SCRIPT_DIR/frameworks/FFmpeg.framework" ] || { echo "error: existing device FFmpeg framework is missing" >&2; exit 1; }

mkdir -p "$BUILD_ROOT"
WORK="$(mktemp -d "$BUILD_ROOT/work.XXXXXXXX")"
trap 'rm -rf "$WORK"' EXIT
STAGE="$WORK/output"
mkdir -p "$STAGE/frameworks"

FFMPEG_COPY="$WORK/ffmpeg-source"
ditto "$FFMPEG_SOURCE" "$FFMPEG_COPY"
rm -rf "$FFMPEG_COPY/build-ios" "$FFMPEG_COPY/build-simulator"
if [ -f "$FFMPEG_COPY/Makefile" ]; then
  make -C "$FFMPEG_COPY" distclean
fi

IOS_SDK_NAME=iphonesimulator \
IOS_DEPLOYMENT_TARGET=16.0 \
IOS_MIN_FLAG=-mios-simulator-version-min=16.0 \
IOS_SUPPORTED_PLATFORM=iPhoneSimulator \
FFMPEG_SOURCE_DIR="$FFMPEG_COPY" \
FFMPEG_BUILD_DIR="$FFMPEG_COPY/build-simulator" \
FFMPEG_FRAMEWORK_OUTPUT_DIR="$WORK/ffmpeg-frameworks" \
bash "$SCRIPT_DIR/build_ffmpeg.sh"

IOS_SDK_NAME=iphonesimulator \
IOS_DEPLOYMENT_TARGET=16.0 \
IOS_MIN_FLAG=-mios-simulator-version-min=16.0 \
ISH_SOURCE_DIR="$ISH_SOURCE" \
ISH_BUILD_DIR="$WORK/ish-build" \
ISH_OUTPUT_LIBS="$STAGE/libs" \
ISH_OUTPUT_INCLUDE="$STAGE/include" \
ISH_OUTPUT_RESOURCES="$STAGE/resources" \
ISH_SKIP_SUBMODULE_UPDATE=1 \
bash "$SCRIPT_DIR/build_ish.sh" release

for framework in FFmpeg libavcodec libavformat libavutil libavfilter libswresample libswscale; do
  xcodebuild -create-xcframework \
    -framework "$SCRIPT_DIR/frameworks/$framework.framework" \
    -framework "$WORK/ffmpeg-frameworks/$framework.framework" \
    -output "$STAGE/frameworks/$framework.xcframework" >/dev/null

  device_binary="$STAGE/frameworks/$framework.xcframework/ios-arm64/$framework.framework/$framework"
  simulator_binary="$STAGE/frameworks/$framework.xcframework/ios-arm64-simulator/$framework.framework/$framework"
  device_build="$(xcrun vtool -show-build "$device_binary")"
  simulator_build="$(xcrun vtool -show-build "$simulator_binary")"
  [[ "$(xcrun lipo -info "$device_binary")" == *arm64* ]] || { echo "error: $framework device slice is not arm64" >&2; exit 1; }
  [[ "$(xcrun lipo -info "$simulator_binary")" == *arm64* ]] || { echo "error: $framework simulator slice is not arm64" >&2; exit 1; }
  [[ "$device_build" == *"platform IOS"* && "$device_build" != *"IOSSIMULATOR"* ]] || { echo "error: $framework device slice has the wrong platform" >&2; exit 1; }
  [[ "$simulator_build" == *"platform IOSSIMULATOR"* && "$simulator_build" == *"minos 16.0"* ]] || { echo "error: $framework simulator slice has the wrong platform or minimum OS" >&2; exit 1; }
done

for library in libish.a libish_emu.a libfakefs.a; do
  binary="$STAGE/libs/$library"
  library_info="$(xcrun lipo -info "$binary")"
  library_build="$(xcrun otool -l "$binary")"
  [[ "$library_info" == *arm64* && "$library_build" == *"platform 7"* && "$library_build" != *"platform 2"* ]] || {
    echo "error: $library is not an arm64 Simulator archive" >&2
    exit 1
  }
done
echo "Simulator slice checks passed"

printf 'Generated locally by build_morde_simulator_dependencies.sh\nSDK: iphonesimulator arm64\nMinimum OS: 16.0\n' > "$STAGE/.morde-simulator-deps"

if [ -e "$OUTPUT_ROOT" ]; then
  [ -f "$OUTPUT_ROOT/.morde-simulator-deps" ] || {
    echo "error: refusing to replace unmarked output at $OUTPUT_ROOT" >&2
    exit 1
  }
  BACKUP="$BUILD_ROOT/previous-$(date +%Y%m%d%H%M%S)"
  mv "$OUTPUT_ROOT" "$BACKUP"
  echo "Previous generated output retained at $BACKUP"
fi
mv "$STAGE" "$OUTPUT_ROOT"
echo "Simulator dependencies ready at $OUTPUT_ROOT"
