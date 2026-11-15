#!/usr/bin/env bash
# A7 spike: agent as guest PID 1 over vsock, virtio-fs shares on idmapped mounts, TAP, and crun in the guest.
# Needs: /tmp/p2/{launcher,agent,usernsexec,crun,busybox}, libkrun in /usr/local/lib64, root, /dev/kvm.
set -uo pipefail

W=/tmp/p2
BASE=200000
result() { echo "RESULT $*"; }

echo "::group::U1 idmapped mounts"
mkdir -p $W/u1/{l,u,w,m,i,j}
echo hi >$W/u1/l/f
chown 1000:1000 $W/u1/l/f
mount -t overlay overlay -o lowerdir=$W/u1/l,upperdir=$W/u1/u,workdir=$W/u1/w $W/u1/m
for spec in "b:0:$BASE:65536" "b:$BASE:0:65536"; do
	if mount --bind -o "X-mount.idmap=$spec" $W/u1/m $W/u1/i; then
		echo "overlay idmap $spec:"
		ls -ln $W/u1/i
		umount $W/u1/i
		result "U1 overlay idmapped bind ($spec) PASS"
	else
		result "U1 overlay idmapped bind ($spec) FAIL"
	fi
done
mkdir -p $W/u1/il $W/u1/iu $W/u1/iw $W/u1/m2 $W/u1/d
echo plain >$W/u1/d/f
chown 1000:1000 $W/u1/d/f
if mount --bind -o "X-mount.idmap=b:0:$BASE:65536" $W/u1/d $W/u1/j; then
	ls -ln $W/u1/j; umount $W/u1/j; result "U1 plain dir idmapped bind PASS"
else
	result "U1 plain dir idmapped bind FAIL"
fi
umount $W/u1/m
mkdir -p $W/u1/all/{l,u,w} $W/u1/allid
echo hi >$W/u1/all/l/f
chown 1000:1000 $W/u1/all/l/f
mount --bind -o "X-mount.idmap=b:0:$BASE:65536" $W/u1/all $W/u1/allid &&
	if mount -t overlay overlay -o lowerdir=$W/u1/allid/l,upperdir=$W/u1/allid/u,workdir=$W/u1/allid/w $W/u1/m2; then
		ls -ln $W/u1/m2; result "U1 overlay over one idmapped parent PASS"
	else
		dmesg | tail -3; result "U1 overlay over one idmapped parent FAIL"
	fi
echo "::endgroup::"

echo "::group::guest root"
R=$W/root
mkdir -p $R/{bin,proc,sys,dev,run,tmp,mnt/shared,etc}
cp $W/agent $R/agent
cp $W/crun $R/bin/crun
cp $W/busybox $R/bin/busybox
for a in $($W/busybox --list); do ln -sf busybox $R/bin/$a; done
cat >$R/.krun_config.json <<'EOF'
{"Cmd":["/agent"],"Env":["PATH=/bin","KRUN_INIT_PID1=1"],"WorkingDir":"/"}
EOF
chmod -R a+rX $R
echo "::endgroup::"

echo "::group::container rootfs and shared dir"
C=$W/ctr
mkdir -p $C/{lower,upper,work,merged}
cp -a $R/bin $C/lower/bin
mkdir -p $C/lower/{proc,sys,dev,etc,data,tmp}
echo "root:x:0:0:root:/root:/bin/sh" >$C/lower/etc/passwd
echo owned >$C/lower/data/f1000
chown 1000:42 $C/lower/data/f1000
chmod 4755 $C/lower/data/f1000
mount -t overlay overlay -o lowerdir=$C/lower,upperdir=$C/upper,workdir=$C/work $C/merged
S=$W/shared
mkdir -p $S
chown $BASE:$BASE $S
echo "::endgroup::"

echo "::group::netns and tap"
ip netns add pod
ip link add veth-host type veth peer name eth0 netns pod
ip addr add 10.88.0.1/24 dev veth-host
ip link set veth-host up
ip netns exec pod ip link set lo up
ip netns exec pod ip addr add 10.88.0.2/24 dev eth0
ip netns exec pod ip link set eth0 up
ip netns exec pod ip tuntap add tap0 mode tap user $BASE group $BASE
ip netns exec pod ip link set tap0 up
ip netns exec pod tc qdisc add dev eth0 ingress
ip netns exec pod tc filter add dev eth0 parent ffff: protocol all u32 match u8 0 0 action mirred egress redirect dev tap0
ip netns exec pod tc qdisc add dev tap0 ingress
ip netns exec pod tc filter add dev tap0 parent ffff: protocol all u32 match u8 0 0 action mirred egress redirect dev eth0
ip netns exec pod ip addr flush dev eth0
MAC=$(ip netns exec pod cat /sys/class/net/eth0/address)
echo "::endgroup::"

SOCK=$S/agent.sock
ip netns exec pod $W/usernsexec $BASE sh -c "grep -E 'Uid|CapEff' /proc/self/status; ls -l /dev/kvm; exec $W/launcher $R $S $SOCK tap0 $MAC" >$W/vm.log 2>&1 &
VM=$!
for _ in $(seq 100); do [ -S $SOCK ] && break; sleep 0.1; done
ls -l $SOCK || { cat $W/vm.log; exit 1; }

# guest: run lines over the agent socket. The vsock connection waits for the guest listener.
guest() {
	python3 - "$SOCK" "$@" <<'EOF'
import socket, sys, time
deadline = time.time() + 20
while True:
    try:
        s = socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])
        s.settimeout(30)
        s.sendall(b"echo ready\n")
        buf = b""
        while b"EXIT" not in buf:
            d = s.recv(4096)
            if not d: raise OSError("closed")
            buf += d
        break
    except OSError as e:
        if time.time() > deadline: print("GUEST connect failed:", e); sys.exit(1)
        time.sleep(0.3)
for line in sys.argv[2:]:
    s.sendall(line.encode() + b"\n")
    buf = b""
    while not buf.rstrip().split(b"\n")[-1].startswith(b"EXIT"):
        d = s.recv(65536)
        if not d: break
        buf += d
    print("GUEST$", line); print(buf.decode(errors="replace").rstrip())
EOF
}

echo "::group::U3 agent over vsock"
if guest 'echo pid1=$(cat /proc/1/comm) self=$$; id; uname -r; mount | grep -E "virtiofs|cgroup2|tmpfs"'; then result "U3 agent PID 1 over vsock PASS"; else result "U3 agent over vsock FAIL"; fi
echo "::endgroup::"

echo "::group::U4 bind after boot into the share, idmapped"
mkdir -p $S/c1/rootfs
chown -R $BASE:$BASE $S/c1
umount $C/merged
mkdir -p $W/ctr-id
mount --bind -o "X-mount.idmap=b:0:$BASE:65536" $C $W/ctr-id
chown $BASE:$BASE $C/upper
mount -t overlay overlay -o lowerdir=$W/ctr-id/lower,upperdir=$C/upper,workdir=$C/work $S/c1/rootfs
ls -ln $S/c1/rootfs/data || result "U4 idmapped bind of container overlay FAIL"
findmnt -o TARGET,FSTYPE,OPTIONS $S/c1/rootfs; dmesg | tail -5
touch $S/c1/rootfs/data/hostnew && ls -ln $S/c1/rootfs/data/hostnew $C/upper/data/hostnew
guest 'touch /mnt/shared/plain && ls -ln /mnt/shared/plain; cat /proc/mounts | grep shared' 'ls -ln /mnt/shared/c1/rootfs/data' 'echo new >/mnt/shared/c1/rootfs/data/new && chown 1000:1000 /mnt/shared/c1/rootfs/data/new && ls -ln /mnt/shared/c1/rootfs/data/new' | tee $W/u4.txt
echo "host upper:"
ls -ln $C/upper/data || true
if grep -q ' 1000 *42 ' $W/u4.txt && [ "$(stat -c %u $C/upper/data/new 2>/dev/null)" = 1000 ]; then result "U4 share after boot with real ownership PASS"; else result "U4 share after boot with real ownership FAIL"; fi
echo "::endgroup::"

echo "::group::U2 TAP networking"
guest "ip link set eth0 address $MAC; ip link set eth0 up; ip addr add 10.88.0.2/24 dev eth0; ip route add default via 10.88.0.1; ip addr show eth0" 'ping -c 2 -W 2 10.88.0.1 && echo PINGOK'  | tee $W/u2.txt
ping -c 2 -W 2 10.88.0.2 && result "U2 ICMP host to guest PASS" || result "U2 ICMP host to guest FAIL"
grep -q PINGOK $W/u2.txt && result "U2 ICMP guest to host PASS" || result "U2 ICMP guest to host FAIL"
echo "::endgroup::"

echo "::group::U5 crun in the guest"
guest 'cd /mnt/shared/c1 && crun spec && sed -i "s/\"terminal\": true/\"terminal\": false/; s|\"sh\"|\"sh\", \"-c\", \"echo hi-from-crun pid=\$\$ host=\$(hostname); id; ls -ln /data\"|" config.json && grep -A3 "\"args\"" config.json' \
	'cd /mnt/shared/c1 && crun --root /run/crun --cgroup-manager=cgroupfs run t1; echo crun-exit=$?' | tee $W/u5.txt
grep -q hi-from-crun $W/u5.txt && result "U5 crun container in the guest PASS" || result "U5 crun container in the guest FAIL"
echo "::endgroup::"

guest 'reboot -f' >/dev/null 2>&1
for _ in $(seq 50); do kill -0 $VM 2>/dev/null || break; sleep 0.2; done
kill -0 $VM 2>/dev/null && { result "VM poweroff FAIL"; kill $VM; } || result "VM poweroff PASS"
echo "::group::vm log"
cat $W/vm.log
echo "::endgroup::"
