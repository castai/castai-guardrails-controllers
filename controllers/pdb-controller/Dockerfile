FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Build context is the repo root so all controllers can share the same
# workflow contract (context: ., file: controllers/<controller>/Dockerfile).
WORKDIR /workspace/controllers/pdb-controller
COPY . /workspace
RUN go mod download
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /workspace/controllers/pdb-controller/castai-pdb-controller-bin ./cmd

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /workspace/controllers/pdb-controller/castai-pdb-controller-bin .
CMD ["./castai-pdb-controller-bin"]
