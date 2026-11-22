# ---------- 构建阶段 ----------
FROM golang:1.26-alpine AS build

WORKDIR /src

# 容器里没有本机的 GOPROXY 配置，默认 proxy.golang.org 在国内基本拉不动。
# 用 ARG 而不是写死 ENV：国外构建可以 --build-arg GOPROXY= 覆盖。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

# 先只拷依赖清单：只改代码时这一层缓存仍然有效，省掉重复下载
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：产物静态链接，才能在 alpine（musl）里直接跑。
# 本机默认 CGO_ENABLED=1 是因为装了 gcc，镜像里不需要，也没有对应的 C 依赖。
# -trimpath 去掉构建机绝对路径；-s -w 去符号表与调试信息，48MB → 约 33MB。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/imchat ./cmd/server

# ---------- 运行阶段 ----------
FROM alpine:3.21

# ca-certificates：访问 OSS 的 HTTPS 需要根证书，alpine 默认没有，
#   缺了会报 x509: certificate signed by unknown authority。
# tzdata：MySQL DSN 写的是 loc=Local，容器默认 UTC，
#   不装时区数据的话读写时间会整体偏 8 小时，且报错里看不出来。
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 imchat

ENV TZ=Asia/Shanghai

COPY --from=build /out/imchat /usr/local/bin/imchat

# 配置只作为结构模板带进去。连接信息（mysql.dsn / redis.addr / rabbitmq.url /
# upload.oss.*）在容器里必须由环境变量覆盖，见 .env.example 与 docs/deploy.md。
# .env 绝不进镜像，已在 .dockerignore 里排除。
COPY config /app/config

WORKDIR /app
USER imchat

EXPOSE 8080

# 注意 /healthz 永远返回 200，依赖是否可用只体现在响应体里
# （见 internal/handler/health.go）。所以这条探针保证的是「进程还能响应 HTTP」，
# 用来抓死锁、goroutine 泄漏这类假活；DB 挂了它不会变成 unhealthy ——
# 那种情况重启容器也没用。
#
# 把 server.healthz 配成 false 会让这条探针拿到 404 而被恒定判为 unhealthy，
# 两者必须一起改（负载均衡那边的健康检查同理）。
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/imchat"]
CMD ["-c", "config/config.yaml"]
