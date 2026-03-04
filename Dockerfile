FROM golang:1.23.0-alpine AS builder

WORKDIR /app/src

COPY src/go.mod ./
RUN go mod download

COPY src/ ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/unifi-hass-verifier .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/unifi-hass-verifier /usr/local/bin/unifi-hass-verifier

USER app

ENTRYPOINT ["unifi-hass-verifier"]
