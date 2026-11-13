#!/usr/bin/env bash
# Usage: run.sh BIN_DIR ROOTFS SCENARIO [gvnet host flags...]
# SCENARIO: normal (guest exits), sigint (Ctrl-C to parent), killchild (SIGKILL to VMM child).
set -euo pipefail

bin_dir=$1 rootfs=$2 scenario=$3
shift 3

log=$(mktemp)
lan_ip=${LAN_IP:?set LAN_IP}
static=1
for a in "$@"; do [[ $a == -dhcp ]] && static=0; done

cp "$bin_dir/guest-linux" "$rootfs/gvnet-guest"
cp "$(dirname "$0")/guest.sh" "$rootfs/gvnet-test.sh"
chmod +x "$rootfs/gvnet-guest" "$rootfs/gvnet-test.sh"
rm -f "$rootfs/gvnet-ready" "$rootfs/gvnet-done"

"$bin_dir/guest-host" serve 127.0.0.1:5555 >/dev/null 2>&1 &
lo_pid=$!
"$bin_dir/guest-host" serve 0.0.0.0:5556 >/dev/null 2>&1 &
lan_pid=$!
trap 'kill $lo_pid $lan_pid 2>/dev/null || true' EXIT

t0=$SECONDS
"$bin_dir/gvnet" host -rootfs "$rootfs" -linger 6s \
  -env "STATIC=$static" -env "LAN_IP=$lan_ip" "$@" -- /bin/sh /gvnet-test.sh >"$log" 2>&1 &
host_pid=$!

for _ in $(seq 1 120); do
  [[ -f $rootfs/gvnet-ready ]] && break
  sleep 0.5
done
t1=$SECONDS
echo "== scenario=$scenario flags=$* ready_after_s=$((t1 - t0))"
child_pid=$(sed -n 's/.*vmm child pid \([0-9]*\).*/\1/p' "$log")

echo "== host curl 127.0.0.1:18080"
curl -sS -m 5 http://127.0.0.1:18080/ || echo "CURL FAIL"

echo "== sockets of parent ($host_pid) and vmm child ($child_pid) while running"
lsof -nP -a -p "$host_pid,$child_pid" -i || true
lsof -nP -a -p "$host_pid,$child_pid" -U || true

case $scenario in
  normal) touch "$rootfs/gvnet-done" ;;
  sigint) kill -INT "$host_pid" ;;
  killchild) kill -KILL "$child_pid" ;;
esac

for _ in $(seq 1 40); do
  grep -q "cleanup done" "$log" && break
  sleep 0.25
done
sleep 0.5
echo "== after vmm exit (parent lingering)"
if kill -0 "$child_pid" 2>/dev/null; then echo "child still alive"; else echo "child gone"; fi
echo "parent inet sockets:"; lsof -nP -a -p "$host_pid" -i || echo "(none)"
echo "parent unix sockets:"; lsof -nP -a -p "$host_pid" -U || echo "(none)"
echo "listeners on 18080:"; lsof -nP -iTCP:18080 -sTCP:LISTEN || echo "(none)"
run_dir=$(sed -n 's/.*run dir removed: \(.*\)$/\1/p' "$log")
echo "run dir $run_dir exists: $([[ -e $run_dir ]] && echo yes || echo no)"
echo "leftover krun-net-$child_pid sockets in TMPDIR: $(ls "${TMPDIR:-/tmp}" 2>/dev/null | grep -c "krun-net-$child_pid-" || true)"

wait "$host_pid" || echo "host exit status $?"
echo "== log"
grep -vE "^time=.*level=(warning|info)" "$log" | grep -v "^\[.*timesync" || true
rm -f "$log"
