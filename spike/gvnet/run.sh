#!/usr/bin/env bash
# Usage: run.sh BIN_DIR ROOTFS LABEL [gvnet host flags...]
# Env: LAN_IP (required), PROBE=1 runs the bypass matrix, HOLD_N=N holds N guest TCP connections to LAN_IP:5558.
set -euo pipefail

bin_dir=$1 rootfs=$2 label=$3
shift 3
lan_ip=${LAN_IP:?set LAN_IP}
log=$(mktemp)

cp "$bin_dir/guest-linux" "$rootfs/gvnet-guest"
cp "$(dirname "$0")/guest.sh" "$rootfs/gvnet-test.sh"
chmod +x "$rootfs/gvnet-guest" "$rootfs/gvnet-test.sh"
rm -f "$rootfs/gvnet-held"

listen=(tcp:127.0.0.1:5555 tcp:0.0.0.0:5557 "tcp:$lan_ip:5558" udp:127.0.0.1:5559)
[[ $(uname -s) == Linux ]] && listen+=(tcp:127.0.0.2:5556)
srv_log=$(mktemp)
LOG_ACCEPT=${PROBE:-0} "$bin_dir/guest-host" listen "${listen[@]}" >"$srv_log" 2>&1 &
srv_pid=$!
trap 'kill $srv_pid 2>/dev/null; wait $srv_pid 2>/dev/null || true' EXIT
sleep 0.5

specs=""
if [[ ${PROBE:-0} == 1 ]]; then
  for ip in 0.0.0.0 127.0.0.1 127.0.0.2 192.168.127.1; do
    specs+=" tcp:$ip:5555 tcp:$ip:5556 tcp:$ip:5557 tcp:$ip:5558 udp:$ip:5559"
  done
  specs+=" tcp:192.168.127.254:5555 tcp:192.168.127.254:5557 udp:192.168.127.254:5559"
  specs+=" tcp:$lan_ip:5555 tcp:$lan_ip:5557 tcp:$lan_ip:5558 udp:$lan_ip:5559"
  specs+=" tcp:169.254.169.254:80 tcp:[::1]:5555 tcp:[::1]:5557 udp:[::1]:5559"
  specs+=" tcp:1.1.1.1:80 tcp:192.168.127.1:53"
fi

fds() {
  if [[ -d /proc/$1/fd ]]; then ls "/proc/$1/fd" | wc -l; else lsof -nP -p "$1" 2>/dev/null | tail -n +2 | wc -l; fi
}
rss_kb() { ps -o rss= -p "$1" | tr -d ' '; }

"$bin_dir/gvnet" host -rootfs "$rootfs" -mode fd -dhcp "$@" \
  -env "LAN_IP=$lan_ip" -env "SPECS=${specs# }" -env "HOLD_N=${HOLD_N:-}" -env "HOLD_ADDR=$lan_ip:5558" -env "HOLD_SECS=15" \
  -- /bin/sh /gvnet-test.sh >"$log" 2>&1 &
host_pid=$!
sleep 1
echo "== $label flags=$* parent_fds_idle=$(fds $host_pid) parent_rss_kb=$(rss_kb $host_pid)"

if [[ -n ${HOLD_N:-} ]]; then
  for _ in $(seq 1 600); do
    [[ -f $rootfs/gvnet-held ]] && break
    kill -0 "$host_pid" 2>/dev/null || break
    sleep 0.5
  done
  sleep 1
  [[ -f /proc/$host_pid/limits ]] && grep "open files" "/proc/$host_pid/limits"
  echo "== holding $(cat "$rootfs/gvnet-held" 2>/dev/null) conns: parent_fds=$(fds $host_pid) parent_rss_kb=$(rss_kb $host_pid) srv_fds=$(fds $srv_pid)"
fi

t_wait=$SECONDS
wait "$host_pid" || echo "host exit status $?"
echo "== parent exited $((SECONDS - t_wait))s after wait started"
grep -E "^(GUEST|PROBE|HOLD)|^host: .*(host addrs|cleanup)" "$log" || true
grep -E "^HOSTSVC" "$srv_log" | sed 's/:[0-9]*$//' | sort | uniq -c || true
rm -f "$log" "$srv_log"
