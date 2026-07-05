#!/bin/bash
set -euo pipefail

# ============================================================
# FusionGate build script — cross-platform, versioned binaries
# ============================================================

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="$PROJECT_DIR/build"
VERSION=$(cat "$PROJECT_DIR/VERSION" 2>/dev/null || echo "dev")

# Platforms to build for
PLATFORMS=(
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)

# ---- clean ----

rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"

echo "============================================"
echo "  FusionGate build v${VERSION}"
echo "============================================"
echo ""

# ---- fusiongate ----

echo ">>> Building fusiongate..."

for platform in "${PLATFORMS[@]}"; do
  IFS="/" read -r GOOS GOARCH <<< "$platform"
  ext=""
  if [ "$GOOS" = "windows" ]; then ext=".exe"; fi

  output="$BUILD_DIR/fusiongate-v${VERSION}-${GOOS}-${GOARCH}${ext}"

  echo "  $GOOS/$GOARCH → $(basename "$output")"

  CGO_ENABLED=0 \
  GOOS="$GOOS" \
  GOARCH="$GOARCH" \
  go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o "$output" \
    ./cmd/fusiongate/
done

# ---- fusiongate-bench ----

echo ""
echo ">>> Building fusiongate-bench..."

for platform in "${PLATFORMS[@]}"; do
  IFS="/" read -r GOOS GOARCH <<< "$platform"
  ext=""
  if [ "$GOOS" = "windows" ]; then ext=".exe"; fi

  output="$BUILD_DIR/fusiongate-bench-v${VERSION}-${GOOS}-${GOARCH}${ext}"

  echo "  $GOOS/$GOARCH → $(basename "$output")"

  CGO_ENABLED=0 \
  GOOS="$GOOS" \
  GOARCH="$GOARCH" \
  go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o "$output" \
    ./cmd/fusiongate-bench/
done

# ---- summary ----

echo ""
echo "============================================"
echo "  Build complete: $(ls "$BUILD_DIR" | wc -l) binaries"
echo "  Version:  v${VERSION}"
echo "  Output:   $BUILD_DIR"
echo "============================================"
ls -lh "$BUILD_DIR"
