# syntax=docker/dockerfile:1

# 1) 前端构建
# NODE_OPTIONS 限制 Vite/tsc/rollup 的堆上限，约束构建期内存峰值。
FROM node:24-alpine AS web
ENV NODE_OPTIONS=--max-old-space-size=2048
RUN corepack enable && corepack prepare pnpm@10 --activate
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# 2) 后端构建（CGO 关闭，SQLite 使用 modernc 纯 Go 驱动）
# GOMAXPROCS=2 限制 go build/test 的并行编译与 GC 线程，约束构建期内存峰值。
FROM golang:1.27 AS gobuild
ENV GOMAXPROCS=2
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/aiops ./cmd/aiops
# 预置空数据目录：运行镜像 distroless 无 shell，借本阶段 mkdir 产物在下一
# 阶段 COPY 过去（属主 65532 = nonroot）。
RUN mkdir -p /out/data

# 3) 运行镜像：distroless static（含 CA 证书），非 root
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=gobuild /out/aiops /app/aiops
COPY --from=web /src/web/dist /app/web/dist
# /app/data 是 named volume 挂载点：卷首次挂载时以镜像内目录的内容与属主
# 初始化卷根。预置属主 65532（nonroot）的空目录，否则卷根属主为 root，
# 进程无法写入 master.key（cryptox: permission denied → 启动崩溃循环）。
COPY --from=gobuild --chown=65532:65532 /out/data /app/data

ENV AIOPS_ADDR=:8080 \
    AIOPS_WEB_DIR=/app/web/dist \
    AIOPS_DATA_DIR=/app/data

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/aiops"]
