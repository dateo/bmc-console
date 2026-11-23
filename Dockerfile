# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bmc-vnc ./cmd/bmc-vnc

# distroless/static ships the CA bundle used to verify BMC HTTPS certificates.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bmc-vnc /bmc-vnc
COPY LICENSE NOTICE NOTICE-firstlight NOTICE-ipmi-connect THIRD_PARTY_NOTICES.md /licenses/
USER nonroot
ENTRYPOINT ["/bmc-vnc"]
