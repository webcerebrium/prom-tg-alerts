FROM golang:1.19-alpine as builder
WORKDIR /apps
ADD go.mod go.sum /apps/
RUN go mod download
ADD cmd/ ./cmd/
ADD internal/ ./internal/
RUN go build -o /apps/bin/prom-tg-alerts ./cmd/prom-tg-alerts

FROM alpine:latest
RUN apk update && apk add ca-certificates && rm -rf /var/cache/apk/*
COPY --from=builder /apps/bin/prom-tg-alerts /apps/bin/prom-tg-alerts
ENTRYPOINT /apps/bin/prom-tg-alerts
