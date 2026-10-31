# ---- 构建阶段：编译 Go 后端（静态二进制，CGO 关闭） ----
FROM golang:1.25 AS build
WORKDIR /src
# 网络受限时可换 Go 模块源（也可经 compose 的 build.args 传入）
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
# 只拷贝编译后端所需的包，避免把 .git / third_party / app 等大目录带进上下文
COPY backend ./backend
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/tad-module ./backend/powerguard

# ---- 运行阶段：最小 Debian + 监控/备份所需工具 ----
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      smartmontools \
      rsync \
      util-linux \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/tad-module /app/tad-module
COPY app/ui/static /app/ui/static
COPY docker/entrypoint.sh /app/entrypoint.sh
COPY docker/config.json /app/config.default.json
RUN chmod 0755 /app/tad-module /app/entrypoint.sh

VOLUME ["/data"]
EXPOSE 8123
ENTRYPOINT ["/app/entrypoint.sh"]
