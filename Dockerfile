FROM golang:1.26.10-alpine3.24 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM alpine:3.24.3

RUN addgroup -g 10001 app \
    && adduser -D -u 10001 -G app app \
    && mkdir -p /var/lib/smart-credit/documents \
    && mkdir -p /var/lib/smart-credit/secrets \
    && chown app:app /var/lib/smart-credit/documents /var/lib/smart-credit/secrets \
    && chmod 0700 /var/lib/smart-credit/secrets /var/lib/smart-credit/documents
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/api /app/api

USER app:app
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["/app/api"]
