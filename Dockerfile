# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

FROM golang:1.26@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=builder /cfgwctl /cfgwctl
USER 65532:65532
ENTRYPOINT ["/cfgwctl"]
