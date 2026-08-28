# ---------- 构建阶段 ----------
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o tg-media-bot ./cmd/tg-media-bot

# ---------- 运行阶段 ----------
FROM alpine:3.20
# rsync/sshpass: rsync 推送；openssh: ssh 连接；tzdata: 时区
RUN apk add --no-cache ca-certificates rsync openssh-client sshpass tzdata
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=builder /build/tg-media-bot /app/tg-media-bot
VOLUME ["/app/data"]
ENTRYPOINT ["/app/tg-media-bot"]
