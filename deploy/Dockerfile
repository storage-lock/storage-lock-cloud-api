# 两阶段构建。基础镜像可通过 build-arg 替换——很多 VPS 直连 Docker Hub 会超时，
# 这时用本地已有的镜像（如 nginx:1.27-alpine）当底座即可：
#
#   docker build --build-arg RUNTIME_BASE=nginx:1.27-alpine ...
#
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO 是 sqlite3 驱动需要的。这里用外部链接器做静态链接，
# 运行期就不依赖目标机的 libc 版本。
RUN CGO_ENABLED=1 go build -trimpath \
      -ldflags="-s -w -linkmode external -extldflags -static" \
      -o /out/cloud-api .

# 运行镜像只需要一个最小的 Linux 环境 + CA 证书。
ARG RUNTIME_BASE=alpine:3.20
FROM ${RUNTIME_BASE}
RUN (command -v apk >/dev/null && apk add --no-cache ca-certificates tzdata || true) \
 && (id -u locks >/dev/null 2>&1 || adduser -D -u 10001 locks || true) \
 && mkdir -p /data && chown locks:locks /data
COPY --from=build /out/cloud-api /usr/local/bin/cloud-api
USER locks
VOLUME ["/data"]
EXPOSE 8080
ENV STORAGE_LOCK_ADDR=":8080" \
    STORAGE_LOCK_DATA="/data/locks.db"
ENTRYPOINT ["/usr/local/bin/cloud-api"]
