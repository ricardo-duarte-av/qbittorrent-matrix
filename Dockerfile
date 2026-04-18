FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go build -o qbittorrent-matrix .

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/qbittorrent-matrix .
ENTRYPOINT ["./qbittorrent-matrix", "config.yaml"]
