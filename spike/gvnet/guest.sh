#!/bin/sh
# Runs inside the VM. The host script reads the GUEST lines and waits on /gvnet-ready.
rm -f /gvnet-ready /gvnet-done

if [ "$STATIC" = 1 ]; then
  ip link set eth0 up
  ip addr add 192.168.127.2/24 dev eth0
  ip route add default via 192.168.127.1
  echo "nameserver 192.168.127.1" > /etc/resolv.conf
fi
echo "GUEST addr: $(ip -4 -o addr show eth0 | awk '{print $4}')"
echo "GUEST route: $(ip route | tr '\n' ';')"
echo "GUEST resolv: $(grep nameserver /etc/resolv.conf | tr '\n' ';')"

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

/gvnet-guest dial 192.168.127.254:5555
/gvnet-guest dial 192.168.127.1:5555
/gvnet-guest dial "$LAN_IP:5556"

# Root guest tries to reach host loopback directly by routing 127.0.0.0/8 out of eth0.
sysctl -q -w net.ipv4.conf.all.route_localnet=1 net.ipv4.conf.eth0.route_localnet=1
ip route del local 127.0.0.1 dev lo table local 2>/dev/null
ip route del local 127.0.0.0/8 dev lo table local 2>/dev/null
ip route add 127.0.0.0/8 via 192.168.127.1 dev eth0 onlink 2>/dev/null
echo "GUEST loroute: $(ip route get 127.0.0.1 2>&1 | head -1)"
/gvnet-guest dial 127.0.0.1:5555

/gvnet-guest serve :8080 &
sleep 1
touch /gvnet-ready
echo "GUEST ready"

i=0
while [ ! -f /gvnet-done ] && [ $i -lt 60 ]; do
  sleep 1
  i=$((i + 1))
done
echo "GUEST exit"
