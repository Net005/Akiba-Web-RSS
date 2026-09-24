# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/akiba-web-rss .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 1000 app && adduser -S -u 1000 -G app app \
 && mkdir /data && chown app:app /data
COPY --from=build /out/akiba-web-rss /usr/local/bin/akiba-web-rss
USER app
WORKDIR /data
VOLUME ["/data"]
EXPOSE 5000
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/akiba-web-rss", "-healthcheck", "-config", "/data/config.json"]
ENTRYPOINT ["/usr/local/bin/akiba-web-rss"]
CMD ["-config", "/data/config.json"]
