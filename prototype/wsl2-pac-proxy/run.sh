#!/usr/bin/env bash
# PROTOTYPE — THROWAWAY. One command to start the #495 prototype.
set -u
export PATH=$PATH:/usr/local/go/bin
cd "$(dirname "$0")/../.."
LOG=${LOG:-/tmp/tbx-pac-proxy.log}
: > "$LOG"
go run ./prototype/wsl2-pac-proxy "$@" >>"$LOG" 2>&1 &
echo "proxy pid $! -> $LOG"
sleep 3
sed -n '1,10p' "$LOG"
