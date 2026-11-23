# syntax=docker/dockerfile:1
#
#   docker build -t bmc-console .                     # bmc-vnc (default)
#   docker build -t bmc-broker --target broker .      # bmc-broker

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# distroless/static ships the CA bundle used to verify BMC HTTPS certificates.
FROM gcr.io/distroless/static-debian12:nonroot AS broker
COPY --from=build /out/bmc-broker /bmc-broker
COPY LICENSE NOTICE NOTICE-firstlight NOTICE-ipmi-connect THIRD_PARTY_NOTICES.md /licenses/
USER nonroot
ENTRYPOINT ["/bmc-broker"]

FROM gcr.io/distroless/static-debian12:nonroot AS bmc-vnc
COPY --from=build /out/bmc-vnc /bmc-vnc
COPY LICENSE NOTICE NOTICE-firstlight NOTICE-ipmi-connect THIRD_PARTY_NOTICES.md /licenses/
USER nonroot
ENTRYPOINT ["/bmc-vnc"]
