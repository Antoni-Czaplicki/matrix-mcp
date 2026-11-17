FROM golang:1.27-alpine AS build
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=1 go build -tags "goolm sqlite_fts5" -trimpath \
    -ldflags "-s -w -linkmode external -extldflags -static" -o /out/matrix-mcp .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 mcp && mkdir /data && chown mcp:mcp /data
COPY --from=build /out/matrix-mcp /usr/local/bin/matrix-mcp
USER mcp
ENV DATA_DIR=/data LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/matrix-mcp"]
