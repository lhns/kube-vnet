# The beacon-ordering experiment's probe (see main.go). Build from this
# directory: docker build -t kube-vnet-probe:e2e test/e2e/probe
FROM golang:1.27 AS builder
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /probe main.go

FROM scratch
COPY --from=builder /probe /probe
USER 65532:65532
ENTRYPOINT ["/probe"]
