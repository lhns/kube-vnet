# syntax=docker/dockerfile:1.6
# --platform=$BUILDPLATFORM keeps the Go toolchain native to the runner's
# architecture even when buildx is producing a multi-arch image. Without
# this, arm64 builds would run the toolchain under QEMU (~5–10× slower).
# The `RUN go build` below cross-compiles via GOOS/GOARCH, so the actual
# binary still targets ${TARGETARCH} correctly.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Version stamps. Set by `docker build --build-arg VERSION=…` (the release
# workflow sets these from the git tag); fall back to placeholders so local
# builds still work.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.date=${BUILD_DATE}" \
      -o /workspace/manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
