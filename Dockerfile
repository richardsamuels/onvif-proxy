# ---- build ----
FROM golang:1.26.5-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/onvif-proxy ./cmd/onvif-proxy

# ---- runtime ----
FROM alpine:3.20
ARG UID=10001
ARG GID=10001
RUN apk add --no-cache ffmpeg tzdata ca-certificates \
    && addgroup -S -g "${GID}" onvif-proxy \
    && adduser -S -D -H -u "${UID}" -G onvif-proxy onvif-proxy \
    && mkdir -p /config \
    && chown onvif-proxy:onvif-proxy /config
COPY --from=build /out/onvif-proxy /usr/local/bin/onvif-proxy
VOLUME /config
EXPOSE 8080
USER onvif-proxy:onvif-proxy
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["sh", "-ec", "kill -0 1"]
ENTRYPOINT ["onvif-proxy", "-config", "/config/config.yaml"]
