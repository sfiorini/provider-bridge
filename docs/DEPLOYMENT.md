# Deployment

Moon Bridge 支持两种部署方式：独立二进制和 Cloudflare Workers WASM。

> 本文档中的基础设施配置（反向代理、Docker Compose 编排等）为示例，请根据实际环境调整。

## 独立二进制部署

### 编译

```bash
go build -o moonbridge ./cmd/moonbridge
```

### 运行

```bash
./moonbridge -config /path/to/config.yml
```

### systemd 服务

```ini
[Unit]
Description=Moon Bridge
After=network.target

[Service]
ExecStart=/usr/local/bin/moonbridge -config /etc/moonbridge/config.yml
Restart=always
RestartSec=5
User=moonbridge

[Install]
WantedBy=multi-user.target
```

### 反向代理（Nginx）

```nginx
server {
    listen 443 ssl;
    server_name moonbridge.example.com;
    location / {
        proxy_pass http://127.0.0.1:38440;
        proxy_set_header Host $host;
        proxy_buffering off;  # 流式响应需要
    }
}
```

## Docker 部署

### Dockerfile（多阶段构建）

```dockerfile
FROM golang:1.27-bookworm AS builder

ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/moonbridge ./cmd/moonbridge

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/moonbridge /app/moonbridge
COPY config.example.yml /app/config.example.yml
EXPOSE 38440
USER nonroot:nonroot
ENTRYPOINT ["/app/moonbridge"]
CMD ["-config", "/config/config.yml", "-addr", "0.0.0.0:38440"]
```

### Docker Compose

```yaml
services:
  moonbridge:
    build: .
    ports: ["38440:38440"]
    volumes:
      - ./config.yml:/config/config.yml
      - ./data:/app/data
```

## Cloudflare Workers WASM

```bash
go build -o worker.wasm ./cmd/cloudflare
```

## 配置管理

- 配置文件通过 `-config` 参数指定
- 运行时通过管理 API（`/api/v1/config`）热重载
- 持久化：默认 SQLite，Cloudflare 环境可用 D1
