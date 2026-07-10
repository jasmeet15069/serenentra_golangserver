FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/hotel-harmony-api ./cmd/server

FROM alpine:3.20
# postgresql16-client provides pg_dump for the per-client backup engine.
RUN apk add --no-cache postgresql16-client \
    && adduser -D -H appuser \
    && mkdir -p /app/backups \
    && chown appuser /app/backups
USER appuser
WORKDIR /app
COPY --from=build /out/hotel-harmony-api /app/hotel-harmony-api
COPY --from=build /src/migrations/ /app/migrations/
EXPOSE 8787
ENTRYPOINT ["/app/hotel-harmony-api"]
