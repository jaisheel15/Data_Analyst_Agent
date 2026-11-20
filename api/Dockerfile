# DuckDB's Go driver needs cgo, so build with gcc and run on glibc.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/api /usr/local/bin/api
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/api"]
