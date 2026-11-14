#!/bin/sh
# Runs inside the VM. The host script reads the GUEST and PROBE lines.
rm -f /gvnet-held
echo "GUEST addr: $(ip -4 -o addr show eth0 | awk '{print $4}') route: $(ip route | tr '\n' ';')"

if nslookup example.com >/tmp/ns 2>&1; then
  echo "GUEST dns: PASS $(grep -A1 'Name:' /tmp/ns | grep Address | head -1)"
else
  echo "GUEST dns: FAIL $(tr '\n' ' ' </tmp/ns)"
fi
if wget -q -T 10 -O /tmp/page http://example.com/; then
  echo "GUEST wget: PASS $(grep -o '<title>[^<]*' /tmp/page)"
else
  echo "GUEST wget: FAIL"
fi
echo "GUEST kernel-dial LAN: $(/gvnet-guest dial "$LAN_IP:5558")"

if [ -n "$SPECS" ]; then
  # shellcheck disable=SC2086
  /gvnet-guest probe 192.168.127.3 $SPECS
fi

if [ -n "$HOLD_N" ]; then
  /gvnet-guest hold "$HOLD_N" "$HOLD_ADDR" "${HOLD_SECS:-15}"
fi
echo "GUEST exit"
