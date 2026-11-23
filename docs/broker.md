# bmc-broker: many BMCs behind one guacd

`bmc-vnc` serves one fixed BMC. That works for a handful of servers, but every
new BMC means another container and a redeploy, and its credentials sit in the
bridge's environment. `bmc-broker` is the alternative for applications that
start console sessions on demand:

```
your app ──(1) POST /v1/tickets {host, credentials} ──────────────────┐
         ──(2) Guacamole VNC connection 127.0.0.1:5901,               │
               password = one-time ticket                             ▼
Guacamole ──► guacd pod [ guacd ──VNC, loopback──► bmc-broker ] ──HTTPS/KVM──► BMC
```

1. Your application, after deciding that the user may open this console, asks
   the broker for a ticket. The request carries the BMC address and
   credentials. The answer is a random 8-character VNC password that works
   once and expires after 60 seconds.
2. The application gives Guacamole a VNC connection to `127.0.0.1:5901` with
   that password, for example through the JSON auth extension.
3. When guacd authenticates, the broker finds the ticket the password belongs
   to, consumes it and opens that BMC's console. Disconnecting ends the BMC
   session.

The BMC credentials only exist in the broker's memory, between the ticket
request and the BMC login. Guacamole's connection record holds the one-time
password, which is useless once the session has started.

## Running it

Run the broker in guacd's network namespace, like `bmc-vnc`. Its VNC listener
only binds to loopback.

```sh
docker build -t bmc-broker --target broker .

export BMC_BROKER_TOKEN=$(openssl rand -hex 24)
bmc-broker \
  -allow-net 10.20.0.0/16 \
  -api-listen :8443 -api-tls-cert tls.crt -api-tls-key tls.key
```

| Flag | Default | Meaning |
|---|---|---|
| `-vnc-listen` | `127.0.0.1:5901` | Loopback address guacd connects to |
| `-api-listen` | `:8080` | Ticket API address |
| `-api-tls-cert`, `-api-tls-key` | | Serve the API over TLS. Ticket requests contain BMC credentials, so use this unless the network path is otherwise protected. |
| `-token-file` | `$BMC_BROKER_TOKEN` | Bearer token for the API, at least 16 characters |
| `-allow-net` | | Networks BMC addresses must be in. Repeatable or comma-separated. Required unless `-allow-any-host` is set. |
| `-allow-any-host` | `false` | Skip the network check. Labs only. |
| `-allow-insecure-bmc-tls` | `false` | Let tickets disable BMC certificate verification |
| `-allow-plain-aten` | `false` | Let tickets use the unencrypted Supermicro ATEN socket |
| `-allow-demo` | `false` | Let tickets open the demo screen instead of a BMC, for testing integrations |
| `-ticket-ttl` | `1m` | Lifetime of an unused ticket |
| `-max-sessions` | `16` | Concurrent console sessions |
| `-max-pending` | `64` | Outstanding tickets |
| `-log-format` | `json` | `json` or `text` |

The network check keeps the broker from being used to reach arbitrary hosts.
Hostnames are resolved and every address must fall into an allowed network.
The BMC connection resolves the name again, so this assumes DNS for the
management network is trusted. Your application should still decide the target
itself and never pass through a host chosen by the browser.

## API

All routes except `/healthz` need `Authorization: Bearer <token>`.

### `POST /v1/tickets`

```json
{
  "mode": "bmc",
  "host": "ilo-rack1.mgmt.example.net",
  "username": "console-operator",
  "password": "...",
  "insecureSkipVerify": false,
  "allowPlainAten": false,
  "user": "alice@example.net",
  "asset": "rack1-node1"
}
```

`mode` is `bmc` (HPE iLO, Dell iDRAC), `aten` (legacy Supermicro) or, with
`-allow-demo`, `demo` (no other fields needed). `user` and `asset` are free-form
labels for the audit log.

`201 Created`:

```json
{"id": "t_9fK2mQx7RtLwP3vA", "vncPassword": "Xq7mKp2R", "expiresAt": "2026-11-23T17:07:23Z"}
```

| Status | Cause |
|---|---|
| 400 | Malformed JSON, unknown field, missing host or credentials, unknown mode |
| 401 | Missing or wrong bearer token |
| 403 | Host outside `-allow-net`, or an option the broker was not started with |
| 409 | This BMC already has an active session or a pending ticket |
| 429 | Too many pending tickets or active sessions |

A BMC can have one ticket or session at a time, because most controllers only
allow one console user. The ticket's `id` names the session once it starts.

### `GET /v1/sessions`

Active sessions: `id`, `host`, `mode`, `user`, `asset`, `startedAt`. No secrets.

### `DELETE /v1/sessions/{id}`

Ends the session, or revokes the ticket if it has not been used yet. `204` on
success, `404` if the ID is unknown.

### `GET /healthz`

`200 ok`, no authentication. Use it for liveness and readiness probes.

## Guacamole connection

| Parameter | Value |
|---|---|
| `protocol` | `vnc` |
| `hostname` | `127.0.0.1` |
| `port` | `5901` (`-vnc-listen`) |
| `password` | the ticket's `vncPassword` |
| `cursor` | `remote` |

Create the connection right after requesting the ticket. guacd has to connect
before the ticket expires.

## Audit log

The broker logs one line per event, in JSON by default: `ticket issued`,
`ticket refused`, `ticket expired`, `ticket revoked`, `session starting`,
`session ended` (with duration), `session terminated` and `viewer rejected`.
Each carries the ticket ID, host, `user` and `asset`. Credentials are never
logged.

## Library use

`pkg/broker` contains the same logic for embedding: `broker.New(Config)`, then
`ServeVNC` on a loopback listener and `Handler(token)` for the API, or call
`Issue`, `Sessions` and `Close` directly.
