# 构建阶段：静态链接，避免在精简镜像里缺 libc。
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO 是 sqlite3 驱动需要的；用 musl 静态链接后运行期不再依赖系统库。
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/cloud-api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 locks
WORKDIR /app
COPY --from=build /out/cloud-api /usr/local/bin/cloud-api
USER locks
# 数据目录必须是可写的挂载点；否则容器里无法落盘。
VOLUME ["/data"]
EXPOSE 8080
ENV STORAGE_LOCK_ADDR=":8080" \
    STORAGE_LOCK_DATA="/data/locks.db"
ENTRYPOINT ["/usr/local/bin/cloud-api"]
