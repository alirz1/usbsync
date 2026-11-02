#!/bin/sh
# Builds USBSync-<version>.spk for DSM 7 on x86_64 (e.g. Synology SA6400 / epyc7002).
# Requires: Go 1.22+, tar, gzip, md5sum.  Usage: ./build.sh
set -e
cd "$(dirname "$0")"
VERSION="1.9.7-0028"
ROOT="$PWD"
OUT="$ROOT/dist"
STAGE="$ROOT/build/stage"
rm -rf "$STAGE" && mkdir -p "$STAGE/pkg/bin" "$STAGE/pkg/ui/images" "$STAGE/pkg/app" "$STAGE/spk" "$OUT"

echo "==> Icons"
python3 tools/icon.py build/icons >/dev/null
cp build/icons/icon_*.png "$STAGE/pkg/ui/images/"
cp build/icons/web_icon.png src/web/icon.png

echo "==> Building binary"
(cd src && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION%-*}" -o "$STAGE/pkg/bin/usbsync" .)

echo "==> package.tgz"
cp spk/ui/config "$STAGE/pkg/ui/config"
cp spk/usbsync.sc "$STAGE/pkg/app/usbsync.sc"
chmod 755 "$STAGE/pkg/bin/usbsync"
tar -C "$STAGE/pkg" --owner=0 --group=0 -czf "$STAGE/spk/package.tgz" bin ui app

echo "==> INFO"
SIZE_KB=$(du -sk "$STAGE/pkg" | cut -f1)
MD5=$(md5sum "$STAGE/spk/package.tgz" | cut -d' ' -f1)
cat > "$STAGE/spk/INFO" <<EOF
package="USBSync"
version="$VERSION"
os_min_ver="7.0-40000"
displayname="USB Sync"
description="Sync NAS shared folders to and from USB/eSATA drives with rsync. Two-pane picker, dry-run previews with a file-by-file report, mirroring, several folder pairs per job, saved jobs with daily/weekly schedules, start-on-plug-in, auto-eject, pause/resume, parallel jobs, resumable big files, and Gmail/Pushbullet notifications."
changelog="Settings backups are now password-protected: passwords and tokens in the exported file are encrypted (AES-256) with a backup password you choose. Import asks for it, or can import without passwords."
maintainer="alirz1"
arch="x86_64 epyc7002"
dsmuidir="ui"
dsmappname="com.alirz.usbsync"
thirdparty="yes"
startable="yes"
ctl_stop="yes"
silent_upgrade="yes"
adminprotocol="http"
adminport="8765"
adminurl="/"
extractsize="$SIZE_KB"
checksum="$MD5"
create_time="$(date +%Y%m%d-%H:%M:%S)"
EOF

cp -r spk/scripts spk/conf "$STAGE/spk/"
chmod 755 "$STAGE/spk/scripts/"*
cp build/icons/PACKAGE_ICON.PNG build/icons/PACKAGE_ICON_256.PNG "$STAGE/spk/"

SPK="$OUT/USBSync-$VERSION-x86_64.spk"
tar -C "$STAGE/spk" --owner=0 --group=0 -cf "$SPK" INFO package.tgz scripts conf PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG
echo "==> $SPK"

echo "==> Source zip (always includes PROJECT_NOTES.md)"
[ -f PROJECT_NOTES.md ] || { echo "PROJECT_NOTES.md is missing"; exit 1; }
ZIP="$OUT/USBSync-${VERSION%-*}-source.zip"
rm -f "$ZIP"
zip -qr "$ZIP" PROJECT_NOTES.md README.md build.sh src spk tools -x 'src/web/icon.png'
echo "==> $ZIP"
