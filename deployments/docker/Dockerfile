FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/hotel-harmony-api ./cmd/server

FROM alpine:3.20
# postgresql16-client provides pg_dump for the per-client backup engine.
# su-exec lets the entrypoint repair ownership of mounted volumes as root, then
# run the API without root privileges.
RUN apk add --no-cache postgresql16-client su-exec \
    && adduser -D -H appuser \
    && mkdir -p /app/backups /app/uploads \
    && chown appuser:appuser /app/backups /app/uploads
WORKDIR /app
COPY --from=build /out/hotel-harmony-api /app/hotel-harmony-api
COPY --from=build /src/migrations/ /app/migrations/
COPY deployments/docker/entrypoint.sh /usr/local/bin/hotel-harmony-entrypoint
RUN chmod 0755 /usr/local/bin/hotel-harmony-entrypoint
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/hotel-harmony-entrypoint"]
