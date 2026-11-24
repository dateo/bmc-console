#!/bin/sh
# Starts Xvnc and a kiosk Chromium on it. The container ends when either one
# exits, which ends the console session.
set -eu

: "${BROWSER_URL:?BROWSER_URL is required}"
: "${VNC_PASSWORD:?VNC_PASSWORD is required}"
width="${BROWSER_WIDTH:-1280}"
height="${BROWSER_HEIGHT:-800}"

umask 077
mkdir -p /tmp/vnc "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME"
printf '%s\n' "$VNC_PASSWORD" | vncpasswd -f > /tmp/vnc/passwd
unset VNC_PASSWORD

# VncAuth only: the broker is the only client, over the cluster network. No
# X11 TCP listener, one shared desktop, and no idle disconnects (the broker
# owns the session lifetime).
Xvnc "$DISPLAY" \
  -geometry "${width}x${height}" -depth 24 \
  -rfbport 5900 -SecurityTypes VncAuth -PasswordFile /tmp/vnc/passwd \
  -AlwaysShared -nolisten tcp -desktop "browser" \
  >/tmp/vnc/xvnc.log 2>&1 &
xvnc=$!

i=0
until [ -e "/tmp/.X11-unix/X${DISPLAY#:}" ]; do
  i=$((i + 1))
  if [ "$i" -gt 100 ] || ! kill -0 "$xvnc" 2>/dev/null; then
    cat /tmp/vnc/xvnc.log >&2
    echo "Xvnc did not start" >&2
    exit 1
  fi
  sleep 0.1
done

set -- \
  --kiosk "$BROWSER_URL" \
  --window-position=0,0 --window-size="${width},${height}" \
  --user-data-dir=/tmp/chromium \
  --no-first-run --no-default-browser-check \
  --disable-sync --disable-translate --disable-background-networking \
  --disable-component-update --disable-default-apps --disable-extensions \
  --disable-features=Translate,MediaRouter,DialMediaRouteProvider \
  --password-store=basic --use-mock-keychain \
  --disable-dev-shm-usage \
  --incognito
if [ "${BROWSER_NO_SANDBOX:-0}" = 1 ]; then
  set -- "$@" --no-sandbox
fi
if [ "${BROWSER_IGNORE_CERT_ERRORS:-0}" = 1 ]; then
  set -- "$@" --ignore-certificate-errors
fi

chromium "$@" &
browser=$!

# Exit as soon as either process ends; tini reaps the other.
while kill -0 "$xvnc" 2>/dev/null && kill -0 "$browser" 2>/dev/null; do
  sleep 1
done
kill "$xvnc" "$browser" 2>/dev/null || true
wait 2>/dev/null || true
