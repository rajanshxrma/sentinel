# Build the manager binary.
#
# The module's go.mod carries a `go 1.26` directive (kubebuilder v4.15
# scaffold / k8s.io deps require it), so the builder stage tracks that
# rather than the go 1.22 base originally sketched for this file — an
# older toolchain simply can't compile a go-1.26-declared module.
FROM golang:1.26 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=arm64

WORKDIR /workspace

# Cache dependencies in their own layer.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the Go source (see .dockerignore for exclusions) and build a static,
# stripped-down binary.
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Distroless nonroot runtime image — no shell, no package manager, minimal
# attack surface for a workload that only needs to talk to the Kubernetes
# API server.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
