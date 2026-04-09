# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

FROM golang:1.26@sha256:ec4debba7b371fb2eaa6169a72fc61ad93b9be6a9ae9da2a010cb81a760d36e7 AS builder
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
