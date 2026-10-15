#!/usr/bin/env bash
#
# Bring up a throwaway TrueNAS in a virtual machine, set it up with what the
# live tests expect to find, and print the environment they need.
#
#   eval "$(.scripts/testenv.sh boot)" # start a NAS as its installer left it, export how to log in
#   eval "$(.scripts/testenv.sh up)"   # the same, set up: export TRUENAS_TEST_* and TRUENAS_EXPORTER_TEST_*
#   .scripts/testenv.sh down           # stop the machine and remove its disks
#   .scripts/testenv.sh logs           # what the machine said and showed, for a failing run
#   .scripts/testenv.sh image          # only install TrueNAS onto a disk, for a cache to keep
#
# TrueNAS has no container and no unattended install that it documents. What it
# has is an installer that answers a small API while it sits at its menu, so:
#
#   1. the machine is started from the installer disc with one empty disk, the
#      installer is told to install onto it, and the machine switched off. That
#      disk is kept, and is all "image" makes. It takes a quarter of an hour
#      and is done once for a version.
#   2. the machine is started from a copy of that disk with four more disks,
#      and a fifth of another kind, which can say how hot it is.
#
# This script only does what the tests cannot do through the NAS's own API,
# which is run the machine. "boot" stops there: the NAS has an administrator
# and nothing else, and making its pool, datasets, users and keys is the live
# tests' job (acceptance/truenasrpc/aa_journey_test.go), since setting a NAS up
# from nothing is the best test of the SDK there is. "up" runs the same steps
# (acceptance/setup) itself, for anything else that wants a NAS that is ready.
#
# What the NAS is made to hold is in acceptance/layout. It needs
# qemu-system-x86_64, qemu-img, curl and go.
#
# TrueNAS runs on x86-64 only. On an x86-64 Linux machine with /dev/kvm the
# machine runs at full speed. Anywhere else it would have to be emulated, which
# is many times slower, so that is refused unless TRUENAS_TEST_ALLOW_EMULATION=1.

set -euo pipefail

VERSION="${TRUENAS_TEST_VERSION:-25.10.7}"
ISO_URL="${TRUENAS_TEST_ISO_URL:-https://download.sys.truenas.net/TrueNAS-SCALE-Goldeye/${VERSION}/TrueNAS-SCALE-${VERSION}.iso}"
# what TrueNAS publishes beside the disc, so that what is installed is what
# TrueNAS released. A different version needs its own given.
ISO_SHA256="${TRUENAS_TEST_ISO_SHA256:-}"
if [ -z "$ISO_SHA256" ] && [ "$VERSION" = "25.10.7" ]; then
  ISO_SHA256="54ce9441ce66966a392e28f63604ca3c2c083d0bec4db7bb5af2f74f7a007c8e"
fi

DATA="${TRUENAS_TEST_DATA:-${HOME}/.cache/truenas-mcp/testenv}"
RUN="${DATA}/run"
ISO="${DATA}/TrueNAS-${VERSION}.iso"
INSTALLED="${DATA}/installed-${VERSION}.qcow2"

PORT="${TRUENAS_TEST_PORT:-18443}"
INSTALLER_PORT="${TRUENAS_TEST_INSTALLER_PORT:-18080}"
MEMORY="${TRUENAS_TEST_MEMORY:-8192}"
CPUS="${TRUENAS_TEST_CPUS:-4}"
# the administrator's password, of a NAS that lives for one run on this machine
export TRUENAS_TEST_PASSWORD="${TRUENAS_TEST_PASSWORD:-truenas-mcp-test}"

# the disk TrueNAS is installed onto is told from the others by its size, so
# no other disk may be this size
BOOT_BYTES=$((16 * 1024 * 1024 * 1024))
DATA_DISK_SIZE="10G"
DATA_DISKS=4
# the serials the data disks are given, numbered from 1: acceptance/layout
# says which of them the pool is made of
DISK_SERIAL="TNTEST"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { echo "==> $*" >&2; }
die() { echo "testenv: $*" >&2; exit 1; }

need() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and is not installed"
  done
}

# how the machine is run: by the processor itself where that is possible
accel() {
  if [ -w /dev/kvm ] && [ "$(uname -m)" = "x86_64" ]; then
    echo "-enable-kvm -cpu host"
  elif [ "${TRUENAS_TEST_ALLOW_EMULATION:-}" = "1" ]; then
    log "no /dev/kvm on an x86-64 machine here: emulating, which is many times slower"
    echo "-accel tcg -cpu max"
  else
    die "TrueNAS needs an x86-64 machine with /dev/kvm to run at a usable speed, and this is not one. Set TRUENAS_TEST_ALLOW_EMULATION=1 to emulate it anyway"
  fi
}

running() { [ -f "${RUN}/vm.pid" ] && kill -0 "$(cat "${RUN}/vm.pid")" 2>/dev/null; }

# machine BOOT_DISK [more qemu arguments] - start the machine in the background.
# The installer's API and the NAS's own are each handed on to a port on this
# machine alone.
machine() {
  local boot=$1
  shift
  local how
  how="$(accel)"

  mkdir -p "$RUN"
  # shellcheck disable=SC2086 # how is two or three separate arguments
  qemu-system-x86_64 \
    -name truenas-mcp-test -machine q35 $how -m "$MEMORY" -smp "$CPUS" \
    -display none -vga std -serial "file:${RUN}/console.log" \
    -monitor "unix:${RUN}/monitor.sock,server,nowait" \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${INSTALLER_PORT}-:8080,hostfwd=tcp:127.0.0.1:${PORT}-:443" \
    -device virtio-net-pci,netdev=net0 \
    -drive "if=none,id=boot,file=${boot},format=qcow2,discard=unmap" \
    -device "virtio-blk-pci,drive=boot,serial=${DISK_SERIAL}BOOT" \
    -pidfile "${RUN}/vm.pid" -daemonize "$@" >"${RUN}/qemu.log" 2>&1 || die "the machine would not start: $(cat "${RUN}/qemu.log")"
}

# stop_machine [SECONDS] - wait that long for the machine to switch itself off,
# then switch it off.
stop_machine() {
  local wait=${1:-0} pid
  [ -f "${RUN}/vm.pid" ] || return 0
  pid="$(cat "${RUN}/vm.pid")"

  while [ "$wait" -gt 0 ] && kill -0 "$pid" 2>/dev/null; do
    sleep 1
    wait=$((wait - 1))
  done

  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    sleep 2
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "${RUN}/vm.pid"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

fetch_iso() {
  [ -n "$ISO_SHA256" ] || die "TRUENAS_TEST_ISO_SHA256 must hold the checksum TrueNAS publishes for the ${VERSION} installer disc"

  if [ ! -f "$ISO" ]; then
    log "fetching the TrueNAS ${VERSION} installer disc (over 2 GB)"
    curl -fL --retry 3 --retry-delay 5 -o "${ISO}.part" "$ISO_URL" >&2
    mv "${ISO}.part" "$ISO"
  fi

  local got
  got="$(sha256 "$ISO")"
  if [ "$got" != "$ISO_SHA256" ]; then
    rm -f "$ISO"
    die "the installer disc is not the one TrueNAS published: its checksum is ${got}, want ${ISO_SHA256}. It was removed"
  fi
}

# image - install TrueNAS onto a disk, once for a version.
image() {
  need qemu-system-x86_64 qemu-img curl go
  mkdir -p "$DATA"

  if [ -f "$INSTALLED" ]; then
    log "TrueNAS ${VERSION} is already installed on ${INSTALLED}"
    return 0
  fi
  running && die "a test machine is already running: .scripts/testenv.sh down"

  fetch_iso

  local disk="${DATA}/installing.qcow2"
  rm -rf "$RUN" "$disk"
  mkdir -p "$RUN"
  qemu-img create -q -f qcow2 "$disk" "$BOOT_BYTES"

  log "starting the machine from the installer disc"
  machine "$disk" -cdrom "$ISO" -boot order=d

  # an emulated machine takes many times as long to get as far as answering
  local answer_within=10m
  [ "${TRUENAS_TEST_ALLOW_EMULATION:-}" = "1" ] && answer_within=60m

  if ! (cd "$REPO" && go run ./acceptance/testnas install -installer "ws://127.0.0.1:${INSTALLER_PORT}/ws" -boot-bytes "$BOOT_BYTES" -answer-within "$answer_within" -timeout 90m); then
    logs || true
    stop_machine
    rm -f "$disk"
    die "TrueNAS ${VERSION} could not be installed"
  fi

  # the installer was told to switch the machine off
  stop_machine 120
  mv "$disk" "$INSTALLED"

  if [ "${TRUENAS_TEST_KEEP_ISO:-1}" != "1" ]; then
    rm -f "$ISO"
  fi
  log "TrueNAS ${VERSION} is installed on ${INSTALLED}"
}

# start HOW - start the machine and wait for the NAS: HOW is "wait", which
# leaves it as its installer left it, or "provision", which sets it up.
start() {
  local how=$1
  need qemu-system-x86_64 qemu-img go
  down
  image

  mkdir -p "$RUN"
  # the installed disk is never written to: the machine runs on a copy that
  # holds only what changed, so every run starts from the same NAS
  qemu-img create -q -f qcow2 -b "$INSTALLED" -F qcow2 "${RUN}/boot.qcow2"

  local disks=() n
  for n in $(seq 1 "$DATA_DISKS"); do
    qemu-img create -q -f qcow2 "${RUN}/data${n}.qcow2" "$DATA_DISK_SIZE"
    disks+=(-drive "if=none,id=data${n},file=${RUN}/data${n}.qcow2,format=qcow2,discard=unmap" -device "virtio-blk-pci,drive=data${n},serial=${DISK_SERIAL}${n}")
  done

  # an NVMe disk says how hot it is, which the others cannot, so whatever
  # reads a disk's temperature has one to read
  qemu-img create -q -f qcow2 "${RUN}/nvme.qcow2" "$DATA_DISK_SIZE"
  disks+=(-drive "if=none,id=nvme,file=${RUN}/nvme.qcow2,format=qcow2,discard=unmap" -device "nvme,drive=nvme,serial=${DISK_SERIAL}NVME")

  log "starting TrueNAS ${VERSION}"
  machine "${RUN}/boot.qcow2" -boot order=c "${disks[@]}"

  # the lines this prints are the environment, and are all that goes to stdout
  if ! (cd "$REPO" && go run ./acceptance/testnas "$how" -server "https://127.0.0.1:${PORT}" -certificate-file "${RUN}/nas.pem" -timeout 30m); then
    logs || true
    die "the TrueNAS did not come up, or could not be set up: the machine is left running to be looked at, and .scripts/testenv.sh down removes it"
  fi
  log "the test NAS is up at https://127.0.0.1:${PORT}"
}

down() {
  stop_machine
  rm -rf "$RUN"
}

# logs - what the machine wrote to its serial port, what QEMU said, and a
# picture of its screen, which is where a TrueNAS that will not start says why.
logs() {
  [ -d "$RUN" ] || { log "no test machine has been run"; return 0; }

  if running && command -v go >/dev/null 2>&1; then
    (cd "$REPO" && go run ./acceptance/testnas screen -monitor "${RUN}/monitor.sock" -file "${RUN}/screen.ppm" -timeout 1m) >&2 || true
  fi

  local file
  for file in qemu.log console.log; do
    if [ -s "${RUN}/${file}" ]; then
      log "${file}:"
      tail -n 200 "${RUN}/${file}" >&2
    fi
  done

  # kept beside the installed disk, where "down" leaves them, for a workflow
  # to save from a failed run
  mkdir -p "${DATA}/logs"
  cp -f "${RUN}/qemu.log" "${RUN}/console.log" "${RUN}/screen.ppm" "${DATA}/logs/" 2>/dev/null || true
}

case "${1:-up}" in
  boot) start wait ;;
  up) start provision ;;
  down) down ;;
  logs) logs ;;
  image) image ;;
  dir) echo "$DATA" ;; # where the installed disk is kept, for a cache to save
  *)
    die "$1: want boot, up, down, logs, image or dir"
    ;;
esac
