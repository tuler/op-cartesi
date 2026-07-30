#!/bin/sh
# Echo app: for every advance, emit one notice (provable output) and one
# report (diagnostic), then accept.
status="accept"
while true; do
  req=$(echo "{\"status\":\"$status\"}" | rollup finish 2>/dev/null) || exit 1
  payload=$(echo "$req" | grep -o '"payload": *"[^"]*"' | head -1 | sed 's/.*"\(0x[^"]*\)".*/\1/')
  [ -z "$payload" ] && payload=0x
  echo "{\"payload\":\"$payload\"}" | rollup notice >/dev/null 2>&1
  echo "{\"payload\":\"$payload\"}" | rollup report >/dev/null 2>&1
  status="accept"
done
