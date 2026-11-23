# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/orchestrator .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates git wget

WORKDIR /app

COPY --from=builder /out/orchestrator /usr/local/bin/orchestrator

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/orchestrator"]
