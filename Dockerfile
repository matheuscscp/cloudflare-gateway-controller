# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

FROM golang:1.26@sha256:fb612b7831d53a89cbc0aaa7855b69ad7b0caf603715860cf538df854d047b84 AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:f512d819b8f109f2375e8b51d8cfd8aafe81034bc3e319740128b7d7f70d5036
COPY --from=builder /cfgwctl /cfgwctl
USER 65532:65532
ENTRYPOINT ["/cfgwctl"]
