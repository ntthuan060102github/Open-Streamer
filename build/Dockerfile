# ---- build stage ----
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /bin/open-streamer \
    ./cmd/server

# ---- runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ffmpeg ca-certificates tzdata

COPY --from=builder /bin/open-streamer /bin/open-streamer

EXPOSE 8080 9090 9091

ENTRYPOINT ["/bin/open-streamer"]
