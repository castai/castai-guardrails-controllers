FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /build/tsc-controller ./cmd/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /build/tsc-controller /tsc-controller
USER nonroot:nonroot
ENTRYPOINT ["/tsc-controller"]
CMD ["--config-namespace=castai-agent"]
