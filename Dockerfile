FROM golang:1.26-alpine AS builder
LABEL maintainer="Chris Thomas <chris.alex.thomas@gmail.com> (@chrisalexthomas)"

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /docker-config-gen ./cmd/docker-config-gen

FROM alpine:3.24

COPY --from=builder /docker-config-gen /usr/local/bin/docker-config-gen

CMD ["docker-config-gen"]
