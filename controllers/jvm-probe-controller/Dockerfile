FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Build context is the repo root so that local-module replace directives
# (../../apis, ../../clientset, ../../snapshot) resolve correctly.
WORKDIR /workspace/controllers/jvm-probe-controller
COPY . /workspace
RUN go mod download

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /workspace/controllers/jvm-probe-controller/jvm-probe-controller ./cmd/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/controllers/jvm-probe-controller/jvm-probe-controller /jvm-probe-controller
USER nonroot:nonroot
ENTRYPOINT ["/jvm-probe-controller"]
CMD ["--config-namespace=castai-agent"]
