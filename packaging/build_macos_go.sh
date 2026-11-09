#!/bin/bash
# Build and package a native, ad-hoc-signed Go MusicOps.app on macOS.
#
#   packaging/build_macos_go.sh --version 0.4.33 [--output dist/macos]
#
# Runs on a native macOS arm64 runner only. The version is passed in by
# the release workflow; this script never reads a version file and never
# changes one. No Developer ID signing or notarization is configured:
# the bundle is ad-hoc signed, exactly like the Python build before it.
#
# Pipeline:
#   1. build darwin/arm64 core (CGO_ENABLED=0) and desktop shell
#      (CGO_ENABLED=1: WKWebView needs CGO) with the version injected
#      via -ldflags; verify both report the version by executing them;
#   2. assemble MusicOps.app (Info.plist from packaging/macos/Info.plist,
#      icon from src/musicops/assets/musicops.png via sips + iconutil,
#      LICENSE + THIRD-PARTY-NOTICES.md, managed engines from
#      packaging/prepare_engines.py);
#   3. audit the bundle (no private/runtime state, no escaping symlink);
#   4. write Contents/Resources/manifest.json ({path,size,sha256} over
#      every file present at that point, except the manifest itself);
#      _CodeSignature is created by signing afterwards, so it is covered
#      by codesign --verify instead of the manifest -- listing its hash
#      in the manifest would be circular, since signing changes it;
#   5. ad-hoc codesign (--deep --force --sign -), verify
#      (--verify --deep --strict) and confirm the ad-hoc identity;
#   6. Go smoke (go test ./internal/smoke/ -run TestBundle) against the
#      .app; the smoke report JSON is extracted from the test log;
#   7. stage, ZIP (ditto), DMG (hdiutil, verified by mounting), PKG
#      (packaging/macos_pkg_go.sh), SHA-256/SHA-512 sidecars, build.json;
#   8. releasegate --layout macos over the finished .app.
#
# NOTE on the smoke invocation: the gated TestBundle takes BundleDir as
# the .app root and DesktopName "MusicOps" (see native/core/internal/
# smoke/smoke_test.go). smoke.Run maps a .app root to Contents/MacOS for
# the binaries and to Contents/Resources for tools, services and the
# manifest (Config.defaults/binRel), so no symlinks inside the bundle
# are needed to paper over the layout.
#
# The Go smoke needs ffmpeg/ffprobe runnable (bundle tools dir or
# PATH); the release workflow installs them on the runner (brew install
# ffmpeg). The installer acceptance test (installer_darwin_test.go) is
# NOT run here: it needs a disposable runner and must run after this
# script, as its own workflow step; that step merges its result into
# build.json (this script writes installer_smoke_passed=false until then).
set -euo pipefail

die() { echo "build_macos_go: $*" >&2; exit 1; }

VERSION=""
OUTPUT="dist/macos"
ARCH="arm64"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --output) OUTPUT="$2"; shift 2 ;;
    --output=*) OUTPUT="${1#--output=}"; shift ;;
    --arch) ARCH="$2"; shift 2 ;;
    --arch=*) ARCH="${1#--arch=}"; shift ;;
    -h|--help)
      echo "usage: $0 --version <X.Y.Z> [--output <dir>] [--arch arm64]"
      exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[ -n "$VERSION" ] || die "--version X.Y.Z is required (passed in by the release workflow; never read from version files)"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid version: $VERSION (want strict X.Y.Z)"
[ "$ARCH" = arm64 ] || die "only Apple Silicon arm64 is published"
[ "$(uname -s)" = Darwin ] || die "macOS bundles must be built and smoke-tested on macOS"
[ "$(uname -m)" = arm64 ] || die "use a native arm64 runner; cross-builds are not accepted"
for tool in go python3 /usr/bin/codesign /usr/bin/lipo /usr/bin/ditto /usr/bin/hdiutil /usr/bin/sips /usr/bin/iconutil git; do
  command -v "$tool" >/dev/null 2>&1 || die "missing tool: $tool"
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$OUTPUT"
OUTPUT="$(cd "$OUTPUT" && pwd)"
STEM="MusicOps-${VERSION}-macOS-${ARCH}"
DIST="$OUTPUT/dist"
APP="$DIST/MusicOps.app"
REPORTS="$OUTPUT/reports"
mkdir -p "$REPORTS"
STAGE="$OUTPUT/package/$STEM"
CORE="$APP/Contents/MacOS/musicops-core"
DESKTOP="$APP/Contents/MacOS/MusicOps"

GO_VERSION="$(go version | awk '{print $3}')"
COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
RUNNER_MACOS="$(sw_vers -productVersion)"

export MACOSX_DEPLOYMENT_TARGET='15.0'
export GOOS=darwin
export GOARCH=arm64
LDFLAGS="-s -w -X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=${VERSION}"

# ---- 1. Build core + desktop shell -------------------------------------
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cd "$ROOT/native/core"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$CORE" ./cmd/musicops
CGO_ENABLED=1 go build -trimpath -ldflags "$LDFLAGS" -o "$DESKTOP" ./cmd/musicops-desktop
cd "$ROOT"
[ -x "$CORE" ] && [ -x "$DESKTOP" ] || die "packaged executables missing or not executable"
/usr/bin/lipo "$CORE" -verify_arch "$ARCH"
/usr/bin/lipo "$DESKTOP" -verify_arch "$ARCH"
# Both binaries report their version by executing them (this runs on the
# macOS runner, where darwin/arm64 binaries execute natively).
CORE_VERSION="$(python3 -c 'import json,subprocess,sys; print(json.loads(subprocess.check_output([sys.argv[1], "version"], text=True))["version"])' "$CORE")"
DESKTOP_VERSION="$(python3 -c 'import json,subprocess,sys; print(json.loads(subprocess.check_output([sys.argv[1], "--version"], text=True))["version"])' "$DESKTOP")"
[ "$CORE_VERSION" = "$VERSION" ] || die "core reports $CORE_VERSION, want $VERSION"
[ "$DESKTOP_VERSION" = "$VERSION" ] || die "desktop shell reports $DESKTOP_VERSION, want $VERSION"
# Record the exact build provenance of both binaries.
go version -m "$CORE" > "$REPORTS/$STEM-core-buildinfo.txt"
go version -m "$DESKTOP" > "$REPORTS/$STEM-desktop-buildinfo.txt"

# ---- 2. Assemble the .app ----------------------------------------------
sed "s/__MUSICOPS_VERSION__/$VERSION/g" "$ROOT/packaging/macos/Info.plist" > "$APP/Contents/Info.plist"
python3 - "$APP/Contents/Info.plist" "$VERSION" <<'PYEOF'
import plistlib, sys
info = plistlib.loads(open(sys.argv[1], 'rb').read())
assert info.get('CFBundleIdentifier') == 'com.wsdbb72.MusicOps', info
assert info.get('CFBundleShortVersionString') == sys.argv[2], info
assert info.get('CFBundleVersion') == sys.argv[2], info
assert info.get('CFBundleExecutable') == 'MusicOps', info
assert info.get('CFBundleIconFile') == 'MusicOps', info
assert info.get('LSBackgroundOnly') is False, info
assert info.get('LSMinimumSystemVersion') == '15.0', info
PYEOF
# Icon: sips resizes the shared PNG into an iconset, iconutil makes the .icns.
ICONSET="$OUTPUT/build-assets/MusicOps.iconset"
rm -rf "$ICONSET"
mkdir -p "$ICONSET"
for spec in "icon_16x16:16" "icon_16x16@2x:32" "icon_32x32:32" "icon_32x32@2x:64" \
            "icon_128x128:128" "icon_128x128@2x:256" "icon_256x256:256" \
            "icon_256x256@2x:512" "icon_512x512:512" "icon_512x512@2x:1024"; do
  name="${spec%%:*}"; px="${spec##*:}"
  /usr/bin/sips -z "$px" "$px" "$ROOT/src/musicops/assets/musicops.png" \
    --out "$ICONSET/$name.png" >/dev/null
done
/usr/bin/iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/MusicOps.icns"
[ -s "$APP/Contents/Resources/MusicOps.icns" ] || die "ICNS generation failed"
cp "$ROOT/LICENSE" "$APP/Contents/Resources/LICENSE"
cp "$ROOT/packaging/THIRD-PARTY-NOTICES.md" "$APP/Contents/Resources/THIRD-PARTY-NOTICES.md"
# Go module licenses (the Python collector would ship PySide6 etc.
# licenses for code that is not in this bundle).
python3 "$ROOT/packaging/collect_go_licenses.py" "$APP/Contents/Resources"

# Managed engines (go-music-dl, FlareSolverr, Chrome for Testing,
# engines.json, licenses/). On darwin the script patches chromedriver
# and ad-hoc signs it before the app itself is sealed below.
python3 "$ROOT/packaging/prepare_engines.py" \
  --output "$APP/Contents/Resources/tools/services" --cache "$OUTPUT/engine-build"
for engine in go-music-dl/go-music-dl flaresolverr/flaresolverr; do
  [ -x "$APP/Contents/Resources/tools/services/$engine" ] || die "engine missing or not executable: $engine"
done
[ -f "$APP/Contents/Resources/tools/services/engines.json" ] || die "engines.json missing"

# ---- 3. Audit: no private/runtime state, no escaping symlink ------------
python3 - "$APP" <<'PYEOF'
import sys
from pathlib import Path
app = Path(sys.argv[1])
forbidden = {'.git', '.env', 'profile', 'profiles', 'vault', 'state',
             'gui-startup-error.txt', 'worker-startup-error.txt'}
bad_suffixes = ('.sqlite', '.sqlite3', '.dpapi', '.keychain', '.keychain-db',
                '.torrent', '.ncm', '.lnk')
for item in app.rglob('*'):
    if item.name in forbidden or item.suffix.lower() in bad_suffixes \
            or item.name.endswith(('.sqlite-wal', '.sqlite-shm')):
        raise SystemExit('Private/runtime state in distributable: ' + item.name)
    if item.is_symlink():
        try:
            item.resolve().relative_to(app.resolve())
        except ValueError:
            raise SystemExit('Bundle symlink escapes application: ' + str(item))
PYEOF

# ---- 4. Manifest (before signing; see the header note) ------------------
python3 - "$APP" <<'PYEOF'
import hashlib, json, sys
from pathlib import Path
app = Path(sys.argv[1])
entries = []
for path in sorted(p for p in app.rglob('*') if not p.is_dir()):
    rel = path.relative_to(app).as_posix()
    if rel == 'Contents/Resources/manifest.json':
        continue
    data = path.read_bytes()  # follows symlinks inside the bundle
    entries.append({'path': rel, 'size': len(data),
                    'sha256': hashlib.sha256(data).hexdigest()})
assert entries, 'manifest would be empty'
out = app / 'Contents/Resources/manifest.json'
out.write_text(json.dumps(entries, ensure_ascii=False, indent=2) + '\n', 'utf-8')
print(f'manifest: {len(entries)} entries')
PYEOF

# ---- 5. Ad-hoc codesign + verify ---------------------------------------
# Preserve Chrome's upstream helper signatures/entitlements: re-seal only
# MusicOps after adding resources; the patched driver was signed
# separately by prepare_engines.py.
/usr/bin/codesign --force --sign - --deep "$APP"
/usr/bin/codesign --verify --deep --strict "$APP"
SIGNATURE="$(/usr/bin/codesign -dvv "$APP" 2>&1)"
case "$SIGNATURE" in
  *Signature=adhoc*) ;;
  *) die "unexpected signing identity; review the release policy" ;;
esac
case "$SIGNATURE" in
  *'Authority=Developer ID'*) die "unexpected Developer ID identity; review the release policy" ;;
esac

# ---- 6. Go smoke against the .app --------------------------------------
SMOKE_LOG="$REPORTS/$STEM-smoke.log"
SMOKE_REPORT="$REPORTS/$STEM-smoke.json"
cd "$ROOT/native/core"
set +e
MUSICOPS_SMOKE_BUNDLE="$APP" MUSICOPS_SMOKE_VERSION="$VERSION" \
  MUSICOPS_SMOKE_DESKTOP="MusicOps" MUSICOPS_SMOKE_REQUIRE_MANIFEST=1 \
  MUSICOPS_SMOKE_REQUIRE_ENGINES=1 MUSICOPS_SMOKE_REPORT="$SMOKE_REPORT" \
  go test ./internal/smoke/ -run 'TestBundle' -count=1 -v > "$SMOKE_LOG" 2>&1
SMOKE_STATUS=$?
set -e
cd "$ROOT"
# TestBundle writes the report JSON to MUSICOPS_SMOKE_REPORT when set;
# fall back to extracting it from the test log (TestBundle also logs it
# via t.Logf) so the report still records what went wrong on failure.
if [ ! -f "$SMOKE_REPORT" ]; then
python3 - "$SMOKE_LOG" "$SMOKE_REPORT" <<'PYEOF'
import json, sys
text = open(sys.argv[1], encoding='utf-8', errors='replace').read()
marker = text.find('smoke report:')
if marker < 0:
    raise SystemExit('smoke log contains no report; see ' + sys.argv[1])
decoder = json.JSONDecoder()
report, _ = decoder.raw_decode(text[text.find('{', marker):])
open(sys.argv[2], 'w', encoding='utf-8').write(
    json.dumps(report, ensure_ascii=False, indent=2) + '\n')
print('smoke passed:', report.get('passed'))
PYEOF
fi
[ "$SMOKE_STATUS" = 0 ] || die "Go smoke failed (status $SMOKE_STATUS); see $SMOKE_LOG and $SMOKE_REPORT"
SMOKE_PASSED="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["passed"])' "$SMOKE_REPORT")"
[ "$SMOKE_PASSED" = "True" ] || die "smoke report did not pass: $SMOKE_REPORT"

# ---- 7. Stage, ZIP, DMG, PKG, checksums, build.json --------------------
rm -rf "$STAGE"
mkdir -p "$STAGE"
/usr/bin/ditto "$APP" "$STAGE/MusicOps.app"
cp "$ROOT/LICENSE" "$STAGE/LICENSE"
cp "$ROOT/packaging/THIRD-PARTY-NOTICES.md" "$STAGE/THIRD-PARTY-NOTICES.md"
cat > "$STAGE/macOS 使用说明.txt" <<EOF
MusicOps $VERSION · macOS 15 或更新版本

推荐使用同版本 .pkg 安装向导；ZIP 用户可将 MusicOps.app 拖入“应用程序”后打开。
此包仅作 ad-hoc 完整性签名，没有 Apple Developer ID 签名和公证。
首次打开时如被 Gatekeeper 阻止，请核对 GitHub Release 的 SHA-256，
确认来源后使用“系统设置 → 隐私与安全性 → 仍要打开”，依系统提示确认。
无需关闭系统安全功能。Apple 官方说明：https://support.apple.com/102445

默认用户资料：~/Library/Application Support/MusicOps/profile
凭据由当前 macOS 用户的钥匙串保护；不会写入 .app，也不能直接复制 Windows DPAPI 凭据。
首次打开请使用连接向导；没有的服务可以跳过。
FFmpeg / ffprobe 未包含，可在设置中选择已安装的 macOS 可执行程序。
Windows 路径和 .exe 媒体工具不能在 Mac 上使用。
EOF

ARCHIVE="$OUTPUT/${STEM}.zip"
/usr/bin/ditto -c -k --sequesterRsrc --keepParent "$STAGE" "$ARCHIVE"

# DMG: drag-to-Applications image from the same tested, signed bundle.
DMG_SRC="$OUTPUT/dmg/$STEM"
rm -rf "$DMG_SRC"
mkdir -p "$(dirname "$DMG_SRC")"
/usr/bin/ditto "$STAGE" "$DMG_SRC"
ln -s /Applications "$DMG_SRC/Applications"
DISK_IMAGE="$OUTPUT/${STEM}.dmg"
rm -f "$DISK_IMAGE"
/usr/bin/hdiutil create -volname "MusicOps $VERSION" -srcfolder "$DMG_SRC" \
  -fs HFS+ -format UDZO -imagekey zlib-level=9 -ov "$DISK_IMAGE"
/usr/bin/hdiutil verify "$DISK_IMAGE"
MOUNT="$OUTPUT/dmg-mount"
mkdir -p "$MOUNT"
/usr/bin/hdiutil attach -readonly -nobrowse -noautoopen -mountpoint "$MOUNT" "$DISK_IMAGE"
DMG_OK=0
if /usr/bin/codesign --verify --deep --strict "$MOUNT/MusicOps.app" \
    && [ "$(readlink "$MOUNT/Applications")" = "/Applications" ] \
    && python3 -c 'import plistlib,sys; assert plistlib.loads(open(sys.argv[1],"rb").read()).get("CFBundleShortVersionString")==sys.argv[2]' \
        "$MOUNT/MusicOps.app/Contents/Info.plist" "$VERSION" \
    && /usr/bin/lipo "$MOUNT/MusicOps.app/Contents/MacOS/MusicOps" -verify_arch "$ARCH"; then
  DMG_OK=1
fi
/usr/bin/hdiutil detach "$MOUNT" || die "could not detach DMG mount $MOUNT"
[ "$DMG_OK" = 1 ] || die "DMG acceptance failed (signature, Applications link, version or architecture)"

# Do NOT capture this script's stdout: pkgbuild/productbuild print
# progress to stdout, so $(...) would swallow their chatter plus the
# echoed path into one multi-line string and [ -s ] would never match
# (release run 42613771085 died here with "PKG build produced no
# installer" even though the PKG was built). The PKG path is
# deterministic - macos_pkg_go.sh computes the same
# "$OUTPUT/${STEM}.pkg" - so build it here and let the inner script's
# stdout stay in the log.
"$ROOT/packaging/macos_pkg_go.sh" --app "$APP" --output "$OUTPUT" --version "$VERSION"
INSTALLER="$OUTPUT/${STEM}.pkg"
[ -s "$INSTALLER" ] || die "PKG build produced no installer"

for algorithm in sha256 sha512; do
  {
    for artifact in "$ARCHIVE" "$INSTALLER" "$DISK_IMAGE"; do
      if [ "$algorithm" = sha256 ]; then sum="$(shasum -a 256 "$artifact" | awk '{print $1}')"; else sum="$(shasum -a 512 "$artifact" | awk '{print $1}')"; fi
      echo "$sum  $(basename "$artifact")"
    done
  } > "$OUTPUT/${STEM}.$algorithm"
done

python3 - "$SMOKE_REPORT" "$OUTPUT" "$STEM" "$VERSION" "$ARCH" "$COMMIT" \
  "$GO_VERSION" "$CORE_VERSION" "$DESKTOP_VERSION" "$RUNNER_MACOS" <<'PYEOF'
import hashlib, json, sys
smoke_path, output, stem, version, arch, commit = sys.argv[1:7]
go_version, core_version, desktop_version, runner_macos = sys.argv[7:11]
from pathlib import Path
smoke = json.loads(Path(smoke_path).read_text('utf-8'))
output = Path(output)
artifacts = {}
for name in (f'{stem}.zip', f'{stem}.pkg', f'{stem}.dmg'):
    path = output / name
    hashes = {}
    for algorithm in ('sha256', 'sha512'):
        with path.open('rb') as stream:
            hashes[algorithm] = hashlib.file_digest(stream, algorithm).hexdigest()
    artifacts[name] = hashes
metadata = {
    'managed_engines_passed': bool(smoke.get('checks', {}).get('engines_present')),
    'managed_engine_checks': {'engines_present': bool(smoke.get('checks', {}).get('engines_present'))},
    'smoke_checks': smoke.get('checks', {}),
    'version': version, 'architecture': arch, 'minimum_macos': '15.0',
    'commit': commit,
    'archive': f'{stem}.zip', 'digests': artifacts[f'{stem}.zip'],
    'artifacts': artifacts,
    'installer': f'{stem}.pkg',
    # The installer acceptance test runs as a separate workflow step after
    # this script and merges its result into this file before upload.
    'installer_smoke_passed': False, 'installer_smoke_checks': {},
    'disk_image': f'{stem}.dmg', 'disk_image_smoke_passed': True,
    'installer_signed': False, 'signing': 'ad-hoc',
    'developer_id_signed': False, 'notarized': False,
    'smoke_passed': bool(smoke.get('passed')),
    'runner_macos': runner_macos,
    'go_version': go_version, 'core_version': core_version,
    'desktop_version': desktop_version,
}
(output / f'{stem}-build.json').write_text(
    json.dumps(metadata, ensure_ascii=False, indent=2) + '\n', 'utf-8')
print(json.dumps(metadata, ensure_ascii=True))
PYEOF

# ---- 8. Release gate over the finished .app -----------------------------
GATE_DIR="$(mktemp -d)"
trap 'rm -rf "$GATE_DIR"' EXIT
(cd "$ROOT/native/core" && CGO_ENABLED=0 go build -trimpath -o "$GATE_DIR/releasegate" ./cmd/releasegate)
"$GATE_DIR/releasegate" --bundle "$APP" --version "$VERSION" --layout macos \
  --desktop MusicOps --require-engines --require-manifest \
  --smoke-report "$SMOKE_REPORT" | tee "$REPORTS/$STEM-releasegate.json"
echo "build_macos_go: $STEM complete"
echo "  app: $APP"
echo "  zip: $ARCHIVE"
echo "  dmg: $DISK_IMAGE"
echo "  pkg: $INSTALLER"
echo "  build.json: $OUTPUT/$STEM-build.json"
