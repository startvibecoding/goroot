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

echo "==> default UX (net / resolv / shell)"
if [ -s /etc/resolv.conf ]; then
  check "host resolv.conf auto-bound" "$(cat /etc/resolv.conf)" "$($BIN run -r "$ROOTFS" -- cat /etc/resolv.conf 2>/dev/null)"
fi
check "default shares host net" "$(ls /sys/class/net | wc -l)" "$($BIN run -r "$ROOTFS" -- /bin/sh -c 'ls /sys/class/net | wc -l' 2>/dev/null)"
check "isolate-net exposes only lo" "1" "$($BIN run -r "$ROOTFS" --isolate-net -- /bin/sh -c 'ls /sys/class/net | wc -l')"
check "auto-selects a shell" "auto-ok" "$(echo 'echo auto-ok; exit' | $BIN run -r "$ROOTFS" 2>/dev/null)"

check "built-in rootfs runs (no -r)" "embedded-ok" "$($BIN run -- /bin/sh -c 'echo embedded-ok' 2>/dev/null)"

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

echo "==> doctor & degradation"
$BIN doctor >/dev/null 2>&1 && ok "doctor runs" || bad "doctor"
out=$(GOROOT_DISABLE_NS=uts $BIN run -r "$ROOTFS" -- hostname 2>/dev/null)
check "uts disabled keeps host hostname" "$(hostname)" "$out"
out=$(GOROOT_DISABLE_NS=pid $BIN run -r "$ROOTFS" -- /bin/sh -c 'ls /proc|grep -cE "^[0-9]+$"' 2>/dev/null)
[ "$out" -gt 20 ] && ok "pid disabled exposes host /proc ($out)" || bad "pid degradation"
if [ "$(id -u)" != 0 ]; then
  GOROOT_DISABLE_NS=user $BIN run -r "$ROOTFS" -- true >/dev/null 2>&1 && bad "should fail without userns" || ok "fails cleanly without userns"
fi

echo

echo "==> daemon (server/client)"
if timeout 10 $BIN client status >/dev/null 2>&1; then
  did=$(timeout 10 $BIN client run -- /bin/sh -c 'echo daemon-hello' 2>/dev/null)
  [ -n "$did" ] && ok "client run returned id ($did)" || bad "client run"
  sleep 1
  check "client logs" "daemon-hello" "$(timeout 10 $BIN client logs "$did" 2>/dev/null)"
  timeout 10 $BIN client ps >/dev/null 2>&1 && ok "client ps" || bad "client ps"
  timeout 10 $BIN client rm "$did" >/dev/null 2>&1 && ok "client rm" || bad "client rm"
  timeout 10 $BIN client shutdown >/dev/null 2>&1 && ok "client shutdown" || bad "client shutdown"
else
  bad "daemon/client unusable"
fi

echo "==> Go SDK"
if go build -o /tmp/goroot-sdk-smoke ./examples/sdk 2>/dev/null; then
  out=$(timeout 30 /tmp/goroot-sdk-smoke 2>/dev/null)
  case "$out" in *"hello from demo"*) ok "sdk run captures output";; *) bad "sdk run (no output)";; esac
  case "$out" in *"exit code 7"*) ok "sdk exit code propagation";; *) bad "sdk exit code";; esac
  rm -f /tmp/goroot-sdk-smoke
else
  bad "sdk example build"
fi
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
