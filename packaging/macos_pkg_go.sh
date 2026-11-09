#!/bin/bash
# Build the native macOS installer PKG for a Go MusicOps.app.
#
#   packaging/macos_pkg_go.sh --app dist/macos/dist/MusicOps.app \
#       --output dist/macos --version 0.4.33
#
# Runs on macOS (arm64) only. The version is passed in by the caller
# (the release workflow); this script never reads a version file.
# Templates live in packaging/macos/: Distribution.xml, components.plist,
# resources/ (Welcome/ReadMe) and scripts/preinstall. License.html is
# generated here from the repository LICENSE at build time.
#
# Steps: stage payload -> pkgbuild component package -> verify the
# component package is readable with `pkgutil --payload-files`
# (before productbuild) -> productbuild the distribution package ->
# verify the finished PKG with `installer -showChoicesXML`.
set -euo pipefail

IDENTIFIER='com.wsdbb72.MusicOps.pkg'
APP_IDENTIFIER='com.wsdbb72.MusicOps'

die() { echo "macos_pkg_go: $*" >&2; exit 1; }

VERSION=""
APP=""
OUTPUT="dist/macos"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --app) APP="$2"; shift 2 ;;
    --app=*) APP="${1#--app=}"; shift ;;
    --output) OUTPUT="$2"; shift 2 ;;
    --output=*) OUTPUT="${1#--output=}"; shift ;;
    -h|--help)
      echo "usage: $0 --app <MusicOps.app> --output <dir> --version <X.Y.Z>"
      exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[ -n "$VERSION" ] || die "--version X.Y.Z is required"
[ -n "$APP" ] || die "--app <MusicOps.app> is required"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid version: $VERSION (want strict X.Y.Z)"
[ "$(uname -s)" = Darwin ] || die "the native installer must be built on macOS"
[ "$(uname -m)" = arm64 ] || die "only Apple Silicon arm64 is published"
for tool in pkgbuild productbuild /usr/sbin/installer /usr/bin/ditto python3; do
  command -v "$tool" >/dev/null 2>&1 || die "missing tool: $tool"
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATES="$ROOT/packaging/macos"
APP="$(cd "$APP" && pwd)"
[ -d "$APP/Contents/MacOS" ] || die "not a macOS app bundle: $APP"
[ -f "$APP/Contents/Info.plist" ] || die "app has no Contents/Info.plist: $APP"
mkdir -p "$OUTPUT"
OUTPUT="$(cd "$OUTPUT" && pwd)"
STEM="MusicOps-${VERSION}-macOS-arm64"
PKG="$OUTPUT/${STEM}.pkg"

BUILD_DIR="$OUTPUT/installer-build"
rm -rf "$BUILD_DIR"
RESOURCES="$BUILD_DIR/resources"
SCRIPTS="$BUILD_DIR/scripts"
mkdir -p "$RESOURCES" "$SCRIPTS"

# Distribution, resources and component list from the checked-in templates.
sed "s/__MUSICOPS_VERSION__/$VERSION/g" "$TEMPLATES/Distribution.xml" > "$BUILD_DIR/Distribution.xml"
sed "s/__MUSICOPS_VERSION__/$VERSION/g" "$TEMPLATES/resources/Welcome.html" > "$RESOURCES/Welcome.html"
cp "$TEMPLATES/resources/ReadMe.html" "$RESOURCES/ReadMe.html"
cp "$TEMPLATES/components.plist" "$BUILD_DIR/components.plist"
python3 - "$ROOT/LICENSE" "$RESOURCES/License.html" <<'PYEOF'
import html, sys
style = '<meta charset="utf-8"><style>body{font:15px -apple-system,sans-serif;color:#182838;line-height:1.6;padding:12px}h1{color:#147b75}code{font-size:13px}pre{white-space:pre-wrap}</style>'
text = open(sys.argv[1], encoding='utf-8').read()
open(sys.argv[2], 'w', encoding='utf-8').write(style + '<pre>' + html.escape(text) + '</pre>')
PYEOF
cp "$TEMPLATES/scripts/preinstall" "$SCRIPTS/preinstall"
chmod 0755 "$SCRIPTS/preinstall"
# The Distribution must reference exactly the component package built below.
grep -q 'MusicOps-component.pkg' "$BUILD_DIR/Distribution.xml" || die "Distribution.xml does not reference MusicOps-component.pkg"
grep -q "$IDENTIFIER" "$BUILD_DIR/Distribution.xml" || die "Distribution.xml does not reference $IDENTIFIER"

# Payload: only the app bundle, exactly as built and signed.
PAYLOAD="$BUILD_DIR/payload"
mkdir -p "$PAYLOAD"
/usr/bin/ditto "$APP" "$PAYLOAD/MusicOps.app"

COMPONENT="$BUILD_DIR/MusicOps-component.pkg"
/usr/bin/pkgbuild --root "$PAYLOAD" --component-plist "$BUILD_DIR/components.plist" \
  --scripts "$SCRIPTS" --identifier "$IDENTIFIER" --version "$VERSION" \
  --install-location /Applications --ownership recommended "$COMPONENT"

# Verify the component package is readable before productbuild wraps it:
# a broken component (bad scripts, bad Distribution reference) must fail
# here, not after the distribution package is produced.
# A pkgbuild component package has no choices at all: installer
# -showChoicesXML on it outputs only the XML declaration (release run
# 42609935556), so asserting a <choice> there can never pass. What proves
# the component is readable is pkgutil opening it and listing its
# payload; the exact 'musicops' choice id is asserted on the finished
# distribution package below, where Distribution.xml defines it.
/usr/sbin/pkgutil --payload-files "$COMPONENT" > "$BUILD_DIR/component-payload.txt" \
  || die "pkgutil cannot read the component package $COMPONENT"
grep -q 'MusicOps.app' "$BUILD_DIR/component-payload.txt" \
  || die "component package payload does not contain MusicOps.app"

rm -f "$PKG"
/usr/bin/productbuild --distribution "$BUILD_DIR/Distribution.xml" \
  --resources "$RESOURCES" --package-path "$BUILD_DIR" "$PKG"
[ -s "$PKG" ] || die "productbuild produced no package"

FINAL_CHOICES="$(/usr/sbin/installer -showChoicesXML -pkg "$PKG" -target /)"
case "$FINAL_CHOICES" in
  *musicops*) ;;
  *) die "installer -showChoicesXML cannot read the finished package $PKG" ;;
esac
echo "$PKG"
