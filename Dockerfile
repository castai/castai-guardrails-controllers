# Dockerfile for combined controllers
# This builds both controllers in a single image

FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git make

WORKDIR /build

# Copy go mod files and download dependencies
WORKDIR /build/tsc-controller
COPY tsc-controller/go.mod tsc-controller/go.sum ./
RUN go mod download

WORKDIR /build/jvm-probe-controller
COPY jvm-probe-controller/go.mod jvm-probe-controller/go.sum ./
RUN go mod download

# Copy source and build
WORKDIR /build/tsc-controller
COPY tsc-controller/cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /bin/tsc-controller ./cmd/

WORKDIR /build/jvm-probe-controller
COPY jvm-probe-controller/cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /bin/jvm-probe-controller ./cmd/

# Final image
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /bin/tsc-controller /usr/local/bin/
COPY --from=builder /bin/jvm-probe-controller /usr/local/bin/

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/tsc-controller"]
