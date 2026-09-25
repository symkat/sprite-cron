FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /sprite-cron ./cmd/sprite-cron

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates util-linux \
    && apt-get clean \
    && groupadd --gid 10001 cronapp && useradd --uid 10001 --gid 10001 --no-create-home cronapp \
    && mkdir /data && chown 10001:10001 /data
COPY --from=build /sprite-cron /usr/local/bin/sprite-cron
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV SPRITE_CRON_DB=/data/sprite-cron.db SPRITE_CRON_LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve"]
