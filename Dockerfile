# syntax=docker/dockerfile:1

FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=0.1.0-alpha.1
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gateway ./cmd/gateway

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gateway /gateway

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/gateway"]
