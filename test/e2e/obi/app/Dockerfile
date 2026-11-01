# syntax=docker/dockerfile:1

FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/e2e-server .

FROM scratch
COPY --from=build /out/e2e-server /app/e2e-server
EXPOSE 8080
ENTRYPOINT ["/app/e2e-server"]
