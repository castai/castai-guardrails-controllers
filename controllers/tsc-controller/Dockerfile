FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Build context is the repo root so that local-module replace directives
# (../../apis, ../../clientset, ../../snapshot) resolve correctly.
WORKDIR /workspace/controllers/tsc-controller
COPY . /workspace
RUN go mod download

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /workspace/controllers/tsc-controller/tsc-controller ./cmd/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/controllers/tsc-controller/tsc-controller /tsc-controller
USER nonroot:nonroot
ENTRYPOINT ["/tsc-controller"]
CMD ["--config-namespace=castai-agent"]
