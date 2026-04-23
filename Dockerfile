# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

FROM golang:1.26@sha256:1e598ea5752ae26c093b746fd73c5095af97d6f2d679c43e83e0eac484a33dc3 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOFIPS140=latest GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /cfgwctl ./cmd/cfgwctl

FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39
COPY --from=builder /cfgwctl /cfgwctl
USER 65532:65532
ENTRYPOINT ["/cfgwctl"]
