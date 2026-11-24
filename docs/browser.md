# Remote browser: a kiosk web browser as a console

> **Status: proof of concept.** The pieces work end to end with the Docker
> launcher; the Kubernetes launcher is unit-tested against a fake API server.
> Read "Security" before exposing it to anyone.

`bmc-broker` can serve a web browser instead of a BMC. A ticket request names
a URL; the broker starts a short-lived container with Chromium in kiosk mode
on its own VNC server, and relays that screen to guacd exactly like a BMC
console:

```
your app ──(1) POST /v1/tickets {mode: browser, url} ─────────────────┐
         ──(2) Guacamole VNC connection 127.0.0.1:5901,               │
               password = one-time ticket                             ▼
Guacamole ──► guacd pod [ guacd ──VNC, loopback──► bmc-broker ] ──VNC──► browser pod ──HTTP(S)──► device
```

Typical use: the web UI of a switch, printer, firewall or BMC that only the
management network can reach. The user's own browser never connects to the
device, and the device never sees the user's machine.

1. The application asks for a ticket with `"mode": "browser"` and the URL.
   The URL's host must be in `-allow-net`, as for a BMC.
2. The broker answers with the one-time VNC password and immediately starts
   the browser container in the background, with a second random VNC
   password that only the broker knows.
3. When guacd connects with the ticket password, the broker waits for the
   container (usually already running), connects to its VNC server and relays
   the screen, keyboard and pointer. Key events pass through as X11 keysyms,
   so every keyboard layout guacd supports works.
4. Disconnecting, an expired or revoked ticket, or `-browser-max-lifetime`
   removes the container.

## The browser image

`browser/` builds a Debian image with TigerVNC's `Xvnc` and Chromium:

```sh
docker build -t bmc-browser browser/
```

It runs as UID 1000 and writes only to `/tmp` and `/dev/shm`, so it works
with a read-only root filesystem. Chromium starts in kiosk mode, incognito,
with a managed policy (`browser/policies.json`) that blocks downloads,
printing, developer tools, extensions, password saving and `file://`,
`chrome://` and `view-source:` URLs.

| Variable | Meaning |
|---|---|
| `BROWSER_URL` | Page to open (required) |
| `VNC_PASSWORD` | VNC password, at most 8 bytes count (required) |
| `BROWSER_WIDTH`, `BROWSER_HEIGHT` | Screen size, default 1280x800 |
| `BROWSER_NO_SANDBOX` | `1` disables Chromium's sandbox; see below |
| `BROWSER_IGNORE_CERT_ERRORS` | `1` accepts self-signed device certificates |

## Running it

```sh
# Local development: containers through the docker CLI.
bmc-broker -allow-net 10.20.0.0/16 \
  -browser-launcher docker -browser-image bmc-browser

# Kubernetes: Pods in the broker's namespace (see the RBAC example).
bmc-broker -allow-net 10.20.0.0/16 \
  -browser-launcher kubernetes -browser-image registry.example/bmc-browser:1 \
  -browser-runtime-class gvisor -browser-no-sandbox \
  -browser-label role=remote-browser
```

| Flag | Default | Meaning |
|---|---|---|
| `-browser-launcher` | off | `kubernetes` or `docker`. Without it, browser tickets are refused. |
| `-browser-image` | | The image built from `browser/` |
| `-browser-namespace` | the broker's own | Namespace for browser Pods |
| `-browser-runtime-class` | | `runtimeClassName`, e.g. `gvisor` or `kata` |
| `-browser-seccomp-profile` | `RuntimeDefault` | Node-local seccomp profile for browser Pods |
| `-browser-apparmor-profile` | runtime default | Node-local AppArmor profile for browser Pods (Kubernetes 1.30+); see "Security" |
| `-browser-no-sandbox` | `false` | Disable Chromium's sandbox; only together with a sandboxing runtime class |
| `-browser-ignore-cert-errors` | `false` | Let the browser accept self-signed certificates |
| `-browser-size` | `1280x800` | Screen size |
| `-browser-max-lifetime` | `1h` | Hard limit of one session |
| `-browser-start-timeout` | `90s` | Time to start a container and reach its VNC server |
| `-browser-label` | | `key=value` label on browser Pods, repeatable (for NetworkPolicy) |
| `-browser-docker-arg` | | Extra `docker run` argument, repeatable |

### Ticket request

```http
POST /v1/tickets
Authorization: Bearer <token>
Content-Type: application/json

{"mode": "browser", "url": "https://10.20.1.15/", "user": "alice", "asset": "switch-7"}
```

The answer and the Guacamole connection are the same as for a BMC. Several
tickets may target the same host at once; unlike a BMC, a web UI has no
single console to take over.

### Kubernetes

Each session is one Pod plus one Secret:

- The **Secret** holds the container's VNC password, so it does not appear
  in the Pod spec. It is owned by the Pod and deleted with it.
- The **Pod** runs non-root with a read-only root filesystem, all
  capabilities dropped, seccomp, no service-account token, no service links,
  and `activeDeadlineSeconds` as a backstop. With `POD_NAME` and `POD_UID`
  set from the downward API, the broker's Pod owns every browser Pod, so
  Kubernetes removes them if the broker goes away.

[`examples/kubernetes/browser-rbac.yaml`](../examples/kubernetes/browser-rbac.yaml)
has the Role the broker needs and a NetworkPolicy for the browser Pods.

## Security

A browser is a large attack surface, and this one runs inside your network.

- **Network reach is the main risk.** A browser Pod can reach whatever its
  network allows: cluster services, the Kubernetes API, cloud metadata. The
  URL check only covers the first page; links and redirects go anywhere.
  Give browser Pods a deny-by-default egress NetworkPolicy that allows only
  the device networks and DNS. The example does this.
- **The browser itself.** Chromium's sandbox needs user namespaces, which the
  default container seccomp profiles block. Either run under a sandboxing
  runtime class (gVisor, Kata) and set `-browser-no-sandbox`, or provide a
  seccomp profile that permits Chromium's sandbox (`clone`/`unshare` with
  namespace flags, `chroot`). On hosts that restrict unprivileged user
  namespaces through AppArmor (`kernel.apparmor_restrict_unprivileged_userns=1`,
  Ubuntu 23.10+), also load an AppArmor profile that allows `userns,` and set
  `-browser-apparmor-profile`; an unconfined profile does not help, and
  turning the sysctl off loosens the whole host. Do not disable the sandbox
  under a plain runc runtime.
- **The broker's own rights.** With the Kubernetes launcher, the broker can
  create Pods and Secrets in one namespace. Use a dedicated namespace.
- **Resources.** Each session costs about 300-500 MB of memory. Bound them
  with `-max-sessions`, the Pod limits and a ResourceQuota.
- **Clipboard** works through Guacamole as for any VNC connection. Downloads
  and printing are blocked by policy; audio is not wired up.

## What was tested

- Unit tests: the VNC client against the repository's own VNC server (VNC
  authentication, frames, input, reconnect while starting), the broker's
  browser tickets with a fake launcher (start on issue, removal on
  disconnect, expiry and revocation, start failures), and the Kubernetes
  launcher against a fake API server (manifest hardening, password only in
  the Secret, cleanup on every failure path).
- `TestDockerEndToEnd` (opt-in): the real image through the Docker launcher.
- By hand: Guacamole 1.5.5 → guacd → `bmc-broker` → browser container →
  an iLO 5 web login page. Pointer and keyboard input arrived; closing the
  tab ended the session and removed the container.
- Chromium's sandbox could not start under Docker's default seccomp on a
  host that restricts unprivileged user namespaces (Ubuntu's AppArmor
  default), so the manual test ran with `BROWSER_NO_SANDBOX=1`.

## Not done yet (proof of concept)

- No idle timeout; only the hard `-browser-max-lifetime`.
- No CJK fonts in the image (`fonts-noto-cjk` adds about 60 MB).
- Tight/ZRLE encodings for the broker-to-browser hop (Hextile is used).
- Per-ticket choice of certificate handling, screen size or allowed hosts
  for navigation.
- A Guacamole round trip in CI.
