#!/bin/sh
# Download a pinned official OpenWrt SDK and verify its SHA-256.
# Usage: fetch-sdk.sh URL OUTPUT SHA256
#
# downloads.openwrt.org is slow from CI and stalls on long transfers. The
# mirrors are probed with a 1 MiB range first and tried fastest first;
# each attempt resumes from the bytes already on disk, gives up when the
# transfer stalls or runs past its time cap, and the next attempt moves to the
# next mirror. The pinned digest makes any mirror safe. An archive that already
# matches the digest, for example from a CI cache, is kept as is.
set -eu

url=$1
out=$2
digest=$3
# Five attempts of 240 s plus probes stay inside the 30 minute job timeout.
attempts=${SDK_FETCH_ATTEMPTS:-5}
attempt_seconds=${SDK_FETCH_ATTEMPT_SECONDS:-240}
# mirror-03.infra.openwrt.org timed out on every probe from GitHub runners.
mirrors="https://downloads.openwrt.org
https://archive.openwrt.org
https://ftp.halifax.rwth-aachen.de/openwrt"

case "$url" in
  https://downloads.openwrt.org/releases/*/openwrt-sdk-*.tar.zst) ;;
  *) echo "fetch-sdk: unexpected SDK URL: $url" >&2; exit 1 ;;
esac
case "$digest" in
  *[!a-f0-9]*) echo "fetch-sdk: invalid SHA-256: $digest" >&2; exit 1 ;;
esac
if [ "${#digest}" -ne 64 ]; then
  echo "fetch-sdk: invalid SHA-256: $digest" >&2
  exit 1
fi
path=${url#https://downloads.openwrt.org/}

verified() {
  printf '%s  %s\n' "$digest" "$out" | sha256sum -c - >/dev/null 2>&1
}

size() {
  if [ -f "$out" ]; then wc -c <"$out" | tr -d ' '; else echo 0; fi
}

if [ -f "$out" ] && verified; then
  echo "fetch-sdk: using verified archive $out ($(size) bytes)"
  exit 0
fi

# Stop a running download if this script is interrupted.
pid=
trap 'if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi' EXIT
trap 'exit 143' INT TERM

# Order mirrors by the speed of a 1 MiB range. Mirrors whose probe failed are
# left out unless every probe failed.
probes=$(for mirror in $mirrors; do
  status=0
  speed=$(curl --http1.1 -sS -fL --proto =https --proto-redir =https -r 0-1048575 \
    --connect-timeout 10 --max-time 20 -o /dev/null -w '%{speed_download}' \
    "$mirror/$path" 2>/dev/null) || status=$?
  speed=${speed%%.*}
  if [ "$status" -ne 0 ]; then speed=0; fi
  echo "fetch-sdk: probe $mirror: $(( ${speed:-0} / 1024 )) KiB/s, curl exit $status" >&2
  echo "${speed:-0} $mirror"
done | sort -rn)
ranked=$(printf '%s\n' "$probes" | awk '$1 > 0 { print $2 }')
if [ -z "$ranked" ]; then ranked=$(printf '%s\n' "$probes" | awk '{ print $2 }'); fi
count=$(printf '%s\n' "$ranked" | wc -l | tr -d ' ')

attempt=1
stalled=0
while [ "$attempt" -le "$attempts" ]; do
  index=$(( (attempt - 1) % count + 1 ))
  source=$(printf '%s\n' "$ranked" | sed -n "${index}p")/$path
  before=$(size)
  echo "fetch-sdk: attempt $attempt from $source at $before bytes"
  curl --http1.1 -sS -fL -C - --proto =https --proto-redir =https --connect-timeout 20 \
    --speed-limit 10240 --speed-time 60 --max-time "$attempt_seconds" \
    -o "$out" "$source" &
  pid=$!
  waited=0
  while kill -0 "$pid" 2>/dev/null; do
    sleep 5
    waited=$((waited + 5))
    if [ $((waited % 30)) -eq 0 ] && kill -0 "$pid" 2>/dev/null; then
      echo "fetch-sdk: attempt $attempt: $(size) bytes after ${waited}s"
    fi
  done
  status=0
  wait "$pid" || status=$?
  pid=
  after=$(size)
  if [ "$status" -eq 0 ]; then
    if verified; then
      echo "fetch-sdk: verified $out after attempt $attempt ($after bytes)"
      exit 0
    fi
    got=$(sha256sum "$out" | cut -c1-64)
    head=$(head -c 48 "$out" | tr -c '[:print:]' '.')
    echo "fetch-sdk: attempt $attempt: SHA-256 mismatch at $after bytes (got $got, starts \"$head\"); restarting from zero" >&2
    rm -f "$out"
    stalled=0
  else
    echo "fetch-sdk: attempt $attempt: curl exit $status from $source after ${waited}s, $before -> $after bytes" >&2
    # Two attempts in a row without progress, such as a 416 on a corrupt
    # complete file, restart from zero; a failed mirror alone keeps the bytes.
    if [ "$after" -gt "$before" ]; then stalled=0; else stalled=$((stalled + 1)); fi
    if [ "$stalled" -ge 2 ]; then
      echo "fetch-sdk: no progress in two attempts; restarting from zero" >&2
      rm -f "$out"
      stalled=0
    fi
  fi
  attempt=$((attempt + 1))
  sleep 5
done
echo "fetch-sdk: giving up after $attempts attempts at $(size) bytes: $url" >&2
exit 1
