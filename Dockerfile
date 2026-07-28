# Stage 1: Build
FROM golang:1.25-alpine AS builder

# 使用国内 Go 模块代理(解决 proxy.golang.org 不可达问题)
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependency downloads
COPY go.mod go.sum ./
COPY sdk/ ./sdk/
RUN go mod download

# Copy source code
COPY . .

# Build all binaries with static linking (single invocation for shared build cache)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/ ./cmd/...

# Install grpc_health_probe for health checks
RUN GRPC_HEALTH_PROBE_VERSION=v0.4.52 && \
    wget -qO /out/grpc_health_probe https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-amd64 && \
    chmod +x /out/grpc_health_probe

# Stage 2: Runtime
FROM alpine:3.20

RUN apk add --no-cache ca-certificates wget && \
    addgroup -g 1000 -S cipherlake && \
    adduser -u 1000 -S cipherlake -G cipherlake

WORKDIR /home/cipherlake

# Copy binaries from builder
COPY --from=builder /out/cipherlake /usr/local/bin/cipherlake
COPY --from=builder /out/cipherlakectl /usr/local/bin/cipherlakectl
COPY --from=builder /out/encrypt-service /usr/local/bin/encrypt-service
COPY --from=builder /out/decrypt-service /usr/local/bin/decrypt-service
COPY --from=builder /out/keygen-service /usr/local/bin/keygen-service
COPY --from=builder /out/keystore-service /usr/local/bin/keystore-service
COPY --from=builder /out/keyunwrap-service /usr/local/bin/keyunwrap-service
COPY --from=builder /out/token-service /usr/local/bin/token-service
COPY --from=builder /out/sts-service /usr/local/bin/sts-service
COPY --from=builder /out/grpc_health_probe /usr/local/bin/grpc_health_probe

# Create data directories
RUN mkdir -p /var/lib/cipherlake/data /var/lib/cipherlake/metadata /var/lib/cipherlake/keystore /etc/cipherlake && \
    chown -R cipherlake:cipherlake /var/lib/cipherlake /etc/cipherlake

USER cipherlake

EXPOSE 9000 9001 9091 50051 50052 50053 50054 50055 50056 50057

VOLUME ["/var/lib/cipherlake", "/etc/cipherlake"]

ENTRYPOINT ["/usr/local/bin/cipherlake"]
CMD ["--config", "/etc/cipherlake/config.yaml"]
