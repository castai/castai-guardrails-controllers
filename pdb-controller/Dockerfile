FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN touch go.sum
COPY cmd/ ./cmd/
RUN go mod tidy && go mod download
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /app/castai-pdb-controller-bin ./cmd

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /app/castai-pdb-controller-bin .
ENTRYPOINT ["/app/castai-pdb-controller-bin"]
