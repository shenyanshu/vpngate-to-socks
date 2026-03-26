FROM golang:1.26-alpine3.22 AS builder

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/vpngate-web . && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/vpngate-runner ./cmd/vpngate-runner

FROM alpine:3.22

RUN apk add --no-cache ca-certificates openvpn

WORKDIR /app

COPY --from=builder /out/vpngate-web /usr/local/bin/vpngate-web
COPY --from=builder /out/vpngate-runner /usr/local/bin/vpngate-runner

ENV PORT=8080

EXPOSE 8080 1080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/health" || exit 1

CMD ["/usr/local/bin/vpngate-web"]
