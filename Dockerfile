# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.22-bookworm AS build
WORKDIR /src

# 先单独拷模块定义，使依赖下载层可缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/server /server
ENV HTTP_ADDR=:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
