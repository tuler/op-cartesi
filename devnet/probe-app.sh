#!/bin/sh
# Probe: report the raw request JSON the guest receives, so the host side can
# be written against what `rollup` actually emits rather than what it is
# assumed to emit.
status="accept"
while true; do
  req=$(echo "{\"status\":\"$status\"}" | rollup finish 2>/dev/null) || exit 1
  hex=$(printf '%s' "$req" | xxd -p | tr -d '\n')
  echo "{\"payload\":\"0x$hex\"}" | rollup report >/dev/null 2>&1
  status="accept"
done
