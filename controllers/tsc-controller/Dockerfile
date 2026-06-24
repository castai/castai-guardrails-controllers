# Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make

# Set working directory
WORKDIR /build

# Copy go.mod first
COPY go.mod ./
RUN touch go.sum

# Copy source code (needed for go mod tidy to detect imports)
COPY cmd/ ./cmd/

# Download dependencies (go mod tidy needs source to detect imports)
RUN go mod tidy && go mod download

# Build the controller
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o tsc-controller ./cmd/

# Final stage - distroless image
FROM gcr.io/distroless/static:nonroot

# Copy binary from builder
COPY --from=builder /build/tsc-controller /tsc-controller

# Use non-root user
USER nonroot:nonroot

# Set entrypoint
ENTRYPOINT ["/tsc-controller"]

# Default arguments
CMD ["--config-namespace=castai-agent"]
