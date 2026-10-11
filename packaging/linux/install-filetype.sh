#!/bin/sh
# Registers the .gocpy file type and its icon for the current user (no root needed).
# Usage: sh install-filetype.sh        (from packaging/linux)
set -eu
cd "$(dirname "$0")"
DATA="${XDG_DATA_HOME:-$HOME/.local/share}"
mkdir -p "$DATA/mime/packages" "$DATA/icons/hicolor/scalable/mimetypes"
cp gocpy-mime.xml "$DATA/mime/packages/gocpy.xml"
cp icons/text-x-gocpy.svg "$DATA/icons/hicolor/scalable/mimetypes/text-x-gocpy.svg"
for s in 16 32 48 64 128 256; do
  mkdir -p "$DATA/icons/hicolor/${s}x${s}/mimetypes"
  cp "icons/text-x-gocpy-$s.png" "$DATA/icons/hicolor/${s}x${s}/mimetypes/text-x-gocpy.png"
done
command -v update-mime-database >/dev/null && update-mime-database "$DATA/mime"
command -v gtk-update-icon-cache >/dev/null && gtk-update-icon-cache -f -t "$DATA/icons/hicolor" 2>/dev/null || true
echo "done: *.gocpy -> text/x-gocpy (restart the file manager if the icon does not appear)"
