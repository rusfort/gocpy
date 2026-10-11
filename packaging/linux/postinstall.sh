#!/bin/sh
# deb/rpm/apk post-install: refresh MIME and icon caches.
command -v update-mime-database >/dev/null && update-mime-database /usr/share/mime || true
command -v gtk-update-icon-cache >/dev/null && gtk-update-icon-cache -f -t /usr/share/icons/hicolor 2>/dev/null || true
