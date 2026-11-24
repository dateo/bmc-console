# syntax=docker/dockerfile:1
#
# Kiosk web browser on a VNC server, started per session by bmc-broker's
# browser launcher (docs/browser.md).
#
#   docker build -t bmc-browser browser/
#
# Environment:
#   BROWSER_URL          page to open (required)
#   VNC_PASSWORD         classic VNC password, at most 8 bytes count (required)
#   BROWSER_WIDTH        screen width, default 1280
#   BROWSER_HEIGHT       screen height, default 800
#   BROWSER_NO_SANDBOX   1 disables Chromium's own sandbox. Only for runtimes
#                        that sandbox the container themselves (gVisor, Kata),
#                        or local tests where seccomp blocks user namespaces.
#   BROWSER_IGNORE_CERT_ERRORS  1 accepts self-signed device certificates.
#
# The image runs as UID 1000 with a read-only root filesystem: everything it
# writes goes to /tmp (and /dev/shm), which the platform provides.

FROM debian:trixie-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium \
      tigervnc-standalone-server \
      tigervnc-tools \
      fonts-dejavu-core \
      fonts-liberation2 \
      fonts-noto-core \
      ca-certificates \
      tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --user-group --home-dir /tmp --no-create-home browser

COPY entrypoint.sh /usr/local/bin/browser-entrypoint
COPY policies.json /etc/chromium/policies/managed/kiosk.json

ENV HOME=/tmp \
    XDG_CONFIG_HOME=/tmp/.config \
    XDG_CACHE_HOME=/tmp/.cache \
    DISPLAY=:1

USER 1000:1000
EXPOSE 5900
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/browser-entrypoint"]
