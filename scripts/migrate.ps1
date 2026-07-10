$ErrorActionPreference = "Stop"

if (-not $env:DATABASE_URL) {
  Write-Error "DATABASE_URL is required"
}

psql $env:DATABASE_URL -f "$PSScriptRoot\..\migrations\001_init.sql"
