# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

FROM golang:1.26 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY main.go main.go
COPY api/ api/
COPY internal/ internal/
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOFIPS140=latest GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /cloudflare-gateway-controller .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /cloudflare-gateway-controller /cloudflare-gateway-controller
USER 65532:65532
ENTRYPOINT ["/cloudflare-gateway-controller"]
