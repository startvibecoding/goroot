#!/usr/bin/env bash
# Smoke test for goroot. Requires unprivileged user namespaces.
set -u

BIN=./goroot
ROOTFS=${ROOTFS:-./rootfs}
IMG=${IMG:-https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz}

pass=0; fail=0
ok()   { echo "  ok   - $1"; pass=$((pass+1)); }
bad()  { echo "  FAIL - $1"; fail=$((fail+1)); }
check(){ # check <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want [$2] got [$3])"; fi
}

[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }

if [ ! -d "$ROOTFS" ]; then
  echo "==> no rootfs at $ROOTFS, extracting alpine"
  "$BIN" extract "$IMG" "$ROOTFS" || { echo "extract failed"; exit 1; }
fi

echo "==> identity"
check "uid is 0 inside" "0" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'id -u')"
check "exit code propagates" "42" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'exit 42'; echo $?)"

echo "==> pid namespace"
check "target is pid 1" "1" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'echo $$')"
npids=$($BIN run -r "$ROOTFS" -- /bin/sh -c 'ls /proc | grep -cE "^[0-9]+$"')
[ "$npids" -lt 20 ] && ok "few processes visible ($npids)" || bad "pid isolation ($npids procs)"
npids_host=$($BIN run -r "$ROOTFS" --no-pid -- /bin/sh -c 'ls /proc | grep -cE "^[0-9]+$"')
[ "$npids_host" -gt 20 ] && ok "--no-pid sees host procs ($npids_host)" || bad "--no-pid ($npids_host)"

echo "==> mount view"
check "proc mounted"  "1" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'grep -q " /proc " /proc/mounts && echo 1')"
check "devpts mounted" "1" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'grep -q " /dev/pts " /proc/mounts && echo 1')"
check "tmpfs on /tmp" "1" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'grep -q " /tmp " /proc/mounts && echo 1')"

echo "==> hostname / uts"
check "hostname set" "goroot" "$($BIN run -r "$ROOTFS" -- hostname)"

echo "==> writable rootfs owned by user"
$BIN run -r "$ROOTFS" -- /bin/sh -c 'echo x > /__w' >/dev/null 2>&1
[ "$(stat -c %u "$ROOTFS/__w" 2>/dev/null)" = "$(id -u)" ] && ok "created file owned by host uid" || bad "file ownership"
rm -f "$ROOTFS/__w"

echo "==> bind mounts"
mkdir -p /tmp/goroot_smoke && echo hi > /tmp/goroot_smoke/f
check "rw bind read" "hi" "$($BIN run -r "$ROOTFS" -b /tmp/goroot_smoke:/mnt/s -- cat /mnt/s/f)"
out=$($BIN run -r "$ROOTFS" -b /etc/hostname:/etc/hostname:ro -- /bin/sh -c 'echo x > /etc/hostname 2>&1 || echo RO')
case "$out" in *"Read-only"*|*"RO"*) ok "ro bind enforced";; *) bad "ro bind ($out)";; esac

echo "==> env / cwd / tmpfs"
check "env passed" "bar" "$($BIN run -r "$ROOTFS" -e FOO=bar -- /bin/sh -c 'echo $FOO')"
check "cwd set" "/tmp" "$($BIN run -r "$ROOTFS" -w /tmp -- pwd)"

echo "==> init mode reaps & propagates"
check "init exit code" "7" "$($BIN run -r "$ROOTFS" --init -- /bin/sh -c 'exit 7'; echo $?)"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
