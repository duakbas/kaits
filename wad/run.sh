#!/usr/bin/env bash
# Start the daemon on localhost. Edit the token to match js/config.js in the app.
# The app connects to ws://localhost:8080/ws — that's the default WAD_ADDR.
export WAD_TOKEN="${WAD_TOKEN:-changeme}"
export WAD_ADDR="${WAD_ADDR:-:8080}"
export WAD_DB="${WAD_DB:-wa-session.db}"
echo "wad: token=$WAD_TOKEN  addr=$WAD_ADDR  db=$WAD_DB"
echo "wad: app should use  ws://localhost${WAD_ADDR}/ws  with the same token"
go run ./cmd/wad
