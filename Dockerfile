FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /server ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -o /producer ./cmd/producer
FROM debian:bookworm-slim
RUN groupadd --system app && useradd --system --gid app app
COPY --from=build /server /usr/local/bin/server
COPY --from=build /producer /usr/local/bin/producer
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
