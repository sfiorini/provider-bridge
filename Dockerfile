FROM golang:1.27-bookworm AS builder

ARG VERSION=dev

ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
    -X providerbridge/internal/service/api.version=${VERSION} \
    -X providerbridge/internal/service/api.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
    -X providerbridge/internal/service/api.goVersion=$(go env GOVERSION)" \
    -o /out/providerbridge ./cmd/providerbridge

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/providerbridge /app/providerbridge
COPY config.example.yml /app/config.example.yml

EXPOSE 38440

USER nonroot:nonroot
ENTRYPOINT ["/app/providerbridge"]
CMD ["-config", "/config/config.yml", "-addr", "0.0.0.0:38440"]
