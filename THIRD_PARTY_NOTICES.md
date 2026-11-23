# Third-party notices

bmc-console is licensed under the Apache License, Version 2.0 (see `LICENSE` and
`NOTICE`). The module has no external Go dependencies (`go.mod` has no
`require` entries), so its binaries contain only the code in this repository and
the Go standard library.

## Code derived from other projects

| Upstream | Revision | License | Used in |
|---|---|---|---|
| [Firstlight](https://github.com/ThatIsCraZy/Firstlight) | [`73de747`](https://github.com/ThatIsCraZy/Firstlight/tree/73de7479468d9565fbd24e554f1bfa48b04bb741) | Apache-2.0 | `pkg/console`, `pkg/idrac`, `pkg/ilo`, `pkg/bmc`, `pkg/kvm`, `pkg/rfb`, `pkg/vmedia`, `pkg/wsx`, `pkg/keyboardmap` |
| [ipmi-connect](https://github.com/mhamzahkhan/ipmi-connect) | [`4c7400d`](https://github.com/mhamzahkhan/ipmi-connect/tree/4c7400de4fd0146bd72fbefb9ea80f9b5dde2d96) | Apache-2.0 | `pkg/aten` |

The upstream NOTICE files are kept verbatim as `NOTICE-firstlight` and
`NOTICE-ipmi-connect`. ipmi-connect's ASPEED video decoder is licensed under
MPL-2.0 and is not part of this repository.

### Changes made to the upstream code

Section 4(b) of the Apache License requires modified files to carry a notice of
the change. This section is that notice.

Code from both projects moved from `internal/` to public packages under `pkg/`,
in the module `github.com/dateo/bmc-console`, with imports rewritten to match.

The Firstlight code gained framebuffer and input methods for the bridge. Legacy
iLO 4 session-cookie handling, redirect handling and a Windows constant were
changed, and a data race in a test double was fixed. The live-test environment
variables were renamed from `FIRSTLIGHT_*` to `BMC_CONSOLE_*`, and so were the
temporary file names in `pkg/keyboardmap`. The keyboard-map authoring guide was
shortened and retitled. Firstlight's GUI, its MCP server and their dependencies
were left out.

The ipmi-connect package was renamed from `supermicro` to `aten`. Its web client
was cut down to the console operations, HTTPS certificate verification became
configurable, and it opens a single ticketed session. The ASPEED decoder was
left out.

`pkg/bridge` and `cmd/bmc-vnc` are original to this project.

## Go standard library

Binaries built from this module statically link the Go standard library, which
is distributed under the following license:

```
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
