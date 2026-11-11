# syntax=docker/dockerfile:1
# moesekai-masterdata-sync 服务镜像（设计文档 §9）：静态编译的 Go 程序 + git。
# 编译阶段在构建机平台上交叉编译，多架构镜像不需要 QEMU。

ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/moesekai-sync ./cmd/moesekai-sync

FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/StarMoe-org/moesekai-masterdata-sync" \
      org.opencontainers.image.description="Syncs Project SEKAI master data into StarMoe-org/moesekai-masterdata-<region>"
# git ≥ 2.44（GIT_NO_LAZY_FETCH）；以非 root 运行，只往 /work 和 /tmp 写（挂 emptyDir）
RUN apk add --no-cache git ca-certificates \
    && adduser -D -H -u 10001 app \
    && git --version
COPY --from=build /out/moesekai-sync /usr/local/bin/moesekai-sync
ENV HOME=/tmp \
    MOESEKAI_WORK_DIR=/work \
    PORT=8080
USER 10001
EXPOSE 8080
ENTRYPOINT ["moesekai-sync"]
CMD ["serve"]
