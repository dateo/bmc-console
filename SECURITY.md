# Security policy

bmc-console holds BMC credentials and opens server consoles, so a bug here can
hand someone firmware-level access to a machine.

## Reporting a vulnerability

Please don't open a public issue. Report it privately through GitHub instead,
under **Security → Report a vulnerability** on this repository. Tell us which
version or commit is affected, how to reproduce the problem and what you think
an attacker could do with it.

We'll confirm we got the report and tell you how the fix is going. If you want
credit in the advisory, you'll get it.

## Scope

Reports about the code in this repository are in scope. That includes the
bridge, the protocol packages and the container image.

Bugs in BMC firmware, Apache Guacamole or guacd belong with their vendor or
project. Problems that only occur when `-insecure-bmc-tls` or
`-allow-plain-aten` is used outside a trusted network are out of scope too.

## Supported versions

Until the first tagged release, only the `main` branch receives fixes.
