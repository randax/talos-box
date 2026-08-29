#!/usr/bin/env bash
# Exercises the public substrate-only workflow from VM creation through
# externally managed Talos configuration, bootstrap, Flannel, and Ready nodes.
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# The override exists only so the contract test can prove the gate fails before
# any setup. CI and normal callers always probe the real device.
kvm_device=${TBX_E2E_KVM_DEVICE:-/dev/kvm}

# This must remain the first operation with any chance of starting a VM.
if [[ "$kvm_device" == /dev/kvm ]]; then
  test -w /dev/kvm
else
  test -w "$kvm_device"
fi

# shellcheck source=scripts/ci/kvm-e2e-lib.sh
source "$root/scripts/ci/kvm-e2e-lib.sh"

required_bytes=$((14 * 1024 * 1024 * 1024))

workdir=""
workdir_owned=false
home=""
helper_socket=""
helper_pid=""
daemon_pid=""
cluster_cleanup_needed=false
# The NFS probe exports a directory from the runner itself, so the export has to
# go away before the workdir holding it does. NFS-Ganesha serves it from
# userspace: CI sandboxes have no nfsd in the kernel and no systemd to manage
# one, so the harness owns the daemon lifecycle directly.
ganesha_pid_file=""
ganesha_log=""
ganesha_running=false
dump_failure_diagnostics() (
  trap - ERR
  set +e
  printf '\n===== talosbox KVM e2e failure diagnostics =====\n' >&2
  for log_file in "$workdir/tbx-helper.log" "$workdir/tbxd.log"; do
    if [[ -f "$log_file" ]]; then
      printf '\n===== %s =====\n' "$(basename "$log_file")" >&2
      cat "$log_file" >&2
    fi
  done
  daemon_ready=false
  if [[ -n "$daemon_pid" && -n "$home" && -S "$home/.talosbox/tbxd.sock" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    daemon_ready=true
    HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" status e2e --quiet -o json >&2
  fi
  if [[ -f "${kubeconfig:-}" ]]; then
    printf '\n===== extension probes =====\n' >&2
    kubectl --kubeconfig "$kubeconfig" get pods -n extensions-e2e -o wide >&2
    kubectl --kubeconfig "$kubeconfig" describe pods -n extensions-e2e >&2
  fi
  if [[ -n "${home:-}" ]]; then
    printf '\n===== guest-agent channel =====\n' >&2
    ls -l "$home"/.talosbox/clusters/e2e/*.qga.sock >&2
    # Who holds the listening socket, and does QEMU carry it as fd 5?
    sudo ss -xlp >&2 | grep -F 'qga.sock' >&2 || true
    for pid in $(pgrep -f qemu-system || true); do
      printf 'qemu pid %s fd 5: ' "$pid" >&2
      sudo readlink "/proc/$pid/fd/5" >&2 || true
    done
    # One verbose attempt with errors visible, so a connect failure is
    # distinguishable from an unanswered ping.
    printf '\xff{"execute":"guest-sync-delimited","arguments":{"id":42}}\n{"execute":"guest-ping"}\n' \
      | socat -d -d -t 10 -T 10 STDIO,ignoreeof "UNIX-CONNECT:$home/.talosbox/clusters/e2e/e2e-cp-1.qga.sock" >&2 || true
  fi
  printf '\n===== host network =====\n' >&2
  sudo ip -details address show >&2
  sudo ip route show table all >&2
  sudo ip neigh show >&2
  sudo bridge link show >&2
  sudo bridge fdb show >&2
  printf '\n===== host firewall =====\n' >&2
  sudo nft list ruleset >&2
  printf '\n===== NFS server =====\n' >&2
  showmount -e localhost >&2 || true
  if [[ -n "${ganesha_log:-}" && -f "$ganesha_log" ]]; then
    sudo cat "$ganesha_log" >&2 || true
  fi
  sudo iptables-save >&2
  printf '\n===== QEMU processes =====\n' >&2
  pgrep -a -f qemu-system >&2
  if [[ "$daemon_ready" == true && -x "$root/bin/tbx" ]]; then
    for node in e2e-cp-1 e2e-worker-1 e2e-worker-2; do
      printf '\n===== console e2e/%s =====\n' "$node" >&2
      timeout 3s "$root/bin/tbx" console e2e "$node" </dev/null >&2
    done
  fi
  printf '\n===== end diagnostics =====\n' >&2
)
cleanup() {
  if [[ "$ganesha_running" == true && -f "$ganesha_pid_file" ]]; then
    sudo kill "$(cat "$ganesha_pid_file")" || true
  fi
  if [[ "$cluster_cleanup_needed" == true && -x "$root/bin/tbx" && -n "$home" && -n "$helper_socket" ]]; then
    HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" cluster destroy e2e --force || true
  fi
  if [[ -n "$daemon_pid" ]]; then
    kill "$daemon_pid" 2>/dev/null || true
  fi
  if [[ -n "$helper_pid" ]]; then
    sudo kill "$helper_pid" 2>/dev/null || true
  fi
  if [[ "$workdir_owned" == true && -f "$workdir/.talosbox-e2e-owned" ]]; then
    rm -rf -- "$workdir"
  fi
}
trap cleanup EXIT
trap dump_failure_diagnostics ERR

prepare_workdir "talosbox-kvm-e2e.XXXXXX"

home="$workdir/home"
helper_socket="$workdir/tbx-helper.sock"
tools="$workdir/tools"
talos_config="$workdir/talos"
cluster_config="$workdir/talosbox.yaml"
kubeconfig="$workdir/kubeconfig"

mkdir -p "$home" "$tools" "$talos_config"
export HOME="$home"
export PATH="$tools:$PATH"
export TBX_HELPER_SOCKET="$helper_socket"

for binary in tbx tbxd tbx-helper; do
  test -x "$root/bin/$binary"
done

# talosctl generates the machine config, so it must match the Talos version the
# cluster boots: every version this harness can boot pins its own checksum. The
# default is the release's tested version; the scheduled floor lane passes the
# bottom of the supported window instead.
talos_version=${TBX_E2E_TALOS_VERSION:-v1.13.6}
case "$talos_version" in
  v1.13.6)
    talosctl_sha256=540c5e7cb0d3fa3a9b2e1c717ced212727b73bcaf0cf9cf9ba2472ec381041d4
    ;;
  v1.12.0)
    talosctl_sha256=11a2745cf92b016b4783acf5eb56bfc394aede61a976dd17b5e8f6d09397e22a
    ;;
  *)
    printf 'no pinned talosctl checksum for Talos %s\n' "$talos_version" >&2
    exit 1
    ;;
esac
kubectl_version=v1.34.1
kubectl_sha256=7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3
flannel_sha256=f17c57f82ffef1d53dbf558ac30755241980563044622778a15df339e4346c57
curl --fail --location --retry 3 --output "$tools/talosctl" \
  "https://github.com/siderolabs/talos/releases/download/${talos_version}/talosctl-linux-amd64"
curl --fail --location --retry 3 --output "$tools/kubectl" \
  "https://dl.k8s.io/release/${kubectl_version}/bin/linux/amd64/kubectl"
curl --fail --location --retry 3 --output "$tools/kube-flannel.yml" \
  https://github.com/flannel-io/flannel/releases/download/v0.27.4/kube-flannel.yml
printf '%s  %s\n' \
  "$talosctl_sha256" "$tools/talosctl" \
  "$kubectl_sha256" "$tools/kubectl" \
  "$flannel_sha256" "$tools/kube-flannel.yml" | sha256sum --check --strict -
chmod +x "$tools/talosctl" "$tools/kubectl"

# The log intentionally remains user-owned so a failed privileged helper is inspectable.
# shellcheck disable=SC2024
sudo env HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx-helper" --allowed-uid "$(id -u)" \
  >"$workdir/tbx-helper.log" 2>&1 &
helper_pid=$!
retry "helper socket" 30 1 socket_ready "$helper_socket"

"$root/bin/tbxd" >"$workdir/tbxd.log" 2>&1 &
daemon_pid=$!
wait_for_process_socket "tbxd" 30 1 "$daemon_pid" "$home/.talosbox/tbxd.sock" "$workdir/tbxd.log"

# No CNI/provisioning keys: talosbox creates substrate only. The explicit role
# sizes keep the Talos control plane above its 2 GiB minimum while preserving
# a modest hosted-runner footprint and sparse 10 GiB raw disks. The full curated
# extension set is requested so every member gets a functional probe below.
cat >"$cluster_config" <<EOF
version: 1
clusters:
  - name: e2e
    controlPlanes: 1
    workers: 2
    talos:
      version: $talos_version
      extensions: [gvisor, nfs-utils, qemu-guest-agent]
    node:
      diskSize: 10GiB
    controlPlane:
      memory: 2GiB
      cpus: 2
    worker:
      memory: 1GiB
      cpus: 1
EOF
cluster_cleanup_needed=true
retry "substrate cluster creation" 3 10 "$root/bin/tbx" up -f "$cluster_config" --force --quiet

mapfile -t node_ips < <(jq -r '.[0].nodes[] | select(.ip != "") | .ip' < <("$root/bin/tbx" status e2e --quiet -o json))
[[ ${#node_ips[@]} -eq 3 ]]
cp_ip=${node_ips[0]}

# The raw images must be sparse, which keeps three nominally 10 GiB disks
# practical on hosted runners while still exercising normal disk provisioning.
for disk in "$home/.talosbox/clusters/e2e"/*.img; do
  apparent=$(stat -c %s "$disk")
  allocated=$(( $(stat -c %b "$disk") * 512 ))
  [[ $allocated -lt $apparent ]]
done

# The probe RPC is only a knock on the door: whether the node answers it or
# rejects it as unimplemented, the maintenance API is serving and the harness
# can move on to apply-config. See maintenance_api_reply_ready in the lib.
maintenance_api_ready() {
  local output status=0
  output=$(talosctl version --nodes "$1" --insecure 2>&1) || status=$?
  if ((status == 0)); then
    return 0
  fi
  maintenance_api_reply_ready "$output"
}
for node_ip in "${node_ips[@]}"; do
  retry "Talos maintenance API at $node_ip" 120 5 maintenance_api_ready "$node_ip"
done

cat >"$talos_config/ci-machine.yaml" <<'EOF'
machine:
  # QEMU inherits the synchronized runner clock. GitHub-hosted runners block
  # guest UDP/123, so do not make this isolated CI cluster wait on public NTP.
  time:
    disabled: true
  # gVisor forks its gofer into new user namespaces, and Talos's KSPP
  # hardening pins user.max_user_namespaces to 0 — runsc then fails at
  # sandbox create with a misleading ENOSPC ("no space left on device").
  # The value mirrors the gvisor extension's documented prerequisite.
  sysctls:
    user.max_user_namespaces: "11255"
cluster:
  network:
    cni:
      name: none
EOF
talosctl gen config e2e "https://${cp_ip}:6443" --output-dir "$talos_config" --config-patch "@$talos_config/ci-machine.yaml"
talosctl apply-config --nodes "$cp_ip" --insecure --file "$talos_config/controlplane.yaml"
for node_ip in "${node_ips[@]:1}"; do
  talosctl apply-config --nodes "$node_ip" --insecure --file "$talos_config/worker.yaml"
done

configured_api_ready() {
  talosctl version --talosconfig "$talos_config/talosconfig" --nodes "$cp_ip" --endpoints "$cp_ip" >/dev/null 2>&1
}
retry "configured Talos API" 120 5 configured_api_ready

bootstrap_cluster() {
  local output
  if output=$(talosctl bootstrap --talosconfig "$talos_config/talosconfig" --nodes "$cp_ip" --endpoints "$cp_ip" 2>&1); then
    return 0
  fi
  if grep -qi 'already bootstrapped' <<<"$output"; then
    return 0
  fi
  printf '%s\n' "$output" >&2
  return 1
}
retry "Talos bootstrap" 12 5 bootstrap_cluster

retrieve_kubeconfig() {
  talosctl kubeconfig "$kubeconfig" --talosconfig "$talos_config/talosconfig" --nodes "$cp_ip" --endpoints "$cp_ip" --force >/dev/null 2>&1
}
retry "kubeconfig retrieval" 120 5 retrieve_kubeconfig

kubernetes_api_ready() {
  kubectl --kubeconfig "$kubeconfig" get --raw=/readyz >/dev/null 2>&1
}
retry "Kubernetes API" 120 5 kubernetes_api_ready

# Flannel is intentionally installed outside talosbox: this proves the
# substrate-only path to Ready nodes.
apply_flannel() {
  kubectl --kubeconfig "$kubeconfig" apply -f "$tools/kube-flannel.yml"
}
retry "Flannel apply" 12 5 apply_flannel

all_nodes_registered() {
  local node_count
  node_count=$(kubectl --kubeconfig "$kubeconfig" get nodes -o name 2>/dev/null | awk 'END { print NR }') || return 1
  [[ "$node_count" -eq 3 ]]
}
retry "node registration" 120 5 all_nodes_registered
kubectl --kubeconfig "$kubeconfig" wait --for=condition=Ready node --all --timeout=10m
ready_nodes=$(kubectl --kubeconfig "$kubeconfig" get nodes --no-headers | awk '$2 == "Ready" { count++ } END { print count + 0 }')
[[ "$ready_nodes" -eq 3 ]]
TBX_E2E_CLUSTER=e2e go test -tags=e2e ./cmd/tbx \
  -run '^TestLinuxDoctorExitCodeWithRunningClusterRoutes$' -count=1
printf 'verified 1 control plane + 2 workers through substrate-only path to Ready nodes\n'

kc() {
  kubectl --kubeconfig "$kubeconfig" "$@"
}

pod_succeeded() {
  [[ "$(kc get pod "$1" -n extensions-e2e -o jsonpath='{.status.phase}' 2>/dev/null)" == Succeeded ]]
}

# Every curated extension gets a functional probe, not a presence check: the
# curated set is closed precisely because each member is provable here.

# The guest-agent socket exists per node only because the cluster requested
# qemu-guest-agent, and only the extension's service inside the guest answers on
# it, so a reply to guest-ping proves both halves of the channel.
qga_socket="$home/.talosbox/clusters/e2e/e2e-cp-1.qga.sock"
test -S "$qga_socket"
guest_ping_answered() {
  local response
  # Two virtio-serial realities shape this probe: a plain pipe's EOF makes
  # socat half-close and QEMU tear down the chardev before qemu-ga's reply
  # lands, so ignoreeof keeps the write half open; and qemu-ga cannot see
  # client disconnects, so a torn-down earlier attempt leaves its JSON parser
  # mid-object — the 0xFF sentinel plus guest-sync-delimited resynchronize it
  # before the ping. Two "return" replies (sync + ping) prove the round trip.
  response=$(printf '\xff{"execute":"guest-sync-delimited","arguments":{"id":42}}\n{"execute":"guest-ping"}\n' \
    | socat -t 10 -T 10 STDIO,ignoreeof "UNIX-CONNECT:$qga_socket" 2>/dev/null) || return 1
  [[ $(grep -c '"return"' <<<"$response") -ge 2 ]]
}
retry "qemu-guest-agent guest-ping" 30 5 guest_ping_answered

# gVisor's sandbox kernel announces itself in the container's own dmesg ring, so
# grepping it from inside the pod proves the runsc handler really ran the
# container instead of silently falling back to runc.
verify_runsc_pod() {
  kc apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: extensions-e2e
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runsc
handler: runsc
---
apiVersion: v1
kind: Pod
metadata:
  name: runsc-probe
  namespace: extensions-e2e
spec:
  runtimeClassName: runsc
  restartPolicy: Never
  containers:
    - name: probe
      image: docker.io/library/busybox:1.37.0
      command:
        - sh
        - -c
        - dmesg | grep -qi gvisor
EOF
  retry "runsc pod completion" 120 5 pod_succeeded runsc-probe
}
verify_runsc_pod

# Talos cannot serve NFS — nfsd is deliberately outside the curated set — so the
# export lives on the runner and the nodes reach it through their own gateway.
# nfsvers=3 is the whole point: without nfs-utils' rpcbind and rpc.statd in the
# image the mount fails outright, and flock over NFSv3 has to travel through NLM.
subnet=$("$root/bin/tbx" status e2e --quiet -o json | jq -r '.[0].subnet')
gateway="${subnet%.0/24}.1"
nfs_export="$workdir/nfs"
mkdir -p "$nfs_export"
chmod 0777 "$nfs_export"
ganesha_conf="$workdir/ganesha.conf"
ganesha_pid_file="$workdir/ganesha.pid"
ganesha_log="$workdir/ganesha.log"
cat > "$ganesha_conf" <<GANESHA
NFS_CORE_PARAM {
  Protocols = 3;
  Enable_NLM = true;
  Enable_RQUOTA = false;
}
NFSV4 {
  Grace_Period = 5;
}
EXPORT {
  Export_Id = 1;
  Path = $nfs_export;
  Pseudo = /e2e;
  Access_Type = RW;
  Squash = No_Root_Squash;
  SecType = sys;
  Protocols = 3;
  Transports = TCP, UDP;
  FSAL { Name = VFS; }
  CLIENT { Clients = $subnet; Access_Type = RW; }
}
GANESHA
# rpcbind first: NFSv3 clients find Ganesha's NFS/NLM services through the
# portmapper. The sysv script wants /run/sendsigs.omit.d; create it so the
# script works with or without systemd, and fall back to launching rpcbind
# directly where no init system manages it.
sudo mkdir -p /run/sendsigs.omit.d
# Ganesha's VFS FSAL enumerates filesystems via /etc/mtab, which minimal CI
# images omit; without it every export fails to create and mounts are denied.
[[ -e /etc/mtab ]] || sudo ln -s /proc/self/mounts /etc/mtab
pgrep -x rpcbind >/dev/null || sudo service rpcbind start || sudo rpcbind
ganesha_running=true
sudo ganesha.nfsd -f "$ganesha_conf" -p "$ganesha_pid_file" -L "$ganesha_log"
retry "Ganesha NFSv3 registration" 12 5 sh -c "rpcinfo -p localhost | grep -q ' nfs' && rpcinfo -p localhost | grep -q nlockmgr"

verify_nfsv3_mount_and_lock() {
  kc apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nfs-probe
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  mountOptions:
    - nfsvers=3
  nfs:
    server: $gateway
    path: $nfs_export
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nfs-probe
  namespace: extensions-e2e
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ""
  volumeName: nfs-probe
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: nfs-probe
  namespace: extensions-e2e
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: docker.io/library/busybox:1.37.0
      command:
        - sh
        - -c
        - flock /data/probe -c 'printf talosbox-nfs-e2e > /data/probe && sync'
      volumeMounts:
        - name: nfs
          mountPath: /data
  volumes:
    - name: nfs
      persistentVolumeClaim:
        claimName: nfs-probe
EOF
  retry "NFSv3 locked write" 120 5 pod_succeeded nfs-probe
  # The readback happens on the server side: the lock and the write both had to
  # reach the export, not just the client's page cache.
  [[ "$(cat "$nfs_export/probe")" == talosbox-nfs-e2e ]]
}
verify_nfsv3_mount_and_lock

kc delete namespace extensions-e2e --wait
kc delete pv nfs-probe --wait
printf 'verified curated extensions: guest-ping, runsc pod, NFSv3 mount with locking\n'
