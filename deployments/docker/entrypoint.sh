#!/bin/sh
set -eu

# Docker creates a new named volume as root:root. That is correct for Docker,
# but this image intentionally runs the API as appuser, so an untouched uploads
# volume otherwise rejects the first guest-ID upload with EACCES. Do the one
# privileged setup operation at startup, then drop privileges for the server.
for dir in /app/backups /app/uploads; do
  mkdir -p "$dir"
  chown appuser:appuser "$dir"
  chmod 0750 "$dir"
done

exec su-exec appuser:appuser /app/hotel-harmony-api
