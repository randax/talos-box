#!/usr/bin/env bash
# Exercises the curated provisioning path with storage end to end: flannel +
# longhorn on a multinode cluster (including replica behavior), then flannel +
# local-path on a single node. PVC bind and write/readback are verified with
# kubectl, independently of tbx's own storage probe.
set -euo pipefail

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

# Two sequential clusters plus the mirror cache holding the curated storage images.
required_bytes=$((20 * 1024 * 1024 * 1024))

workdir=""
workdir_owned=false
home=""
helper_socket=""
helper_pid=""
daemon_pid=""
cluster_cleanup_needed=false
diagnostics_cluster=e2e-storage
dump_failure_diagnostics() (
  trap - ERR
  set +e
  printf '\n===== talosbox KVM storage e2e failure diagnostics =====\n' >&2
  for log_file in "$workdir/tbx-helper.log" "$workdir/tbxd.log"; do
    if [[ -f "$log_file" ]]; then
      printf '\n===== %s =====\n' "$(basename "$log_file")" >&2
      cat "$log_file" >&2
    fi
  done
  daemon_ready=false
  if [[ -n "$daemon_pid" && -n "$home" && -S "$home/.talosbox/tbxd.sock" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
    daemon_ready=true
    HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" status "$diagnostics_cluster" --quiet -o json >&2
  fi
  kubeconfig="$home/.talosbox/clusters/$diagnostics_cluster/kubeconfig"
  if [[ -f "$kubeconfig" ]]; then
    printf '\n===== cluster workloads =====\n' >&2
    kubectl --kubeconfig "$kubeconfig" get nodes -o wide >&2
    kubectl --kubeconfig "$kubeconfig" get pods --all-namespaces -o wide >&2
    kubectl --kubeconfig "$kubeconfig" get pvc --all-namespaces >&2
    kubectl --kubeconfig "$kubeconfig" get events --all-namespaces --sort-by=.lastTimestamp | tail -n 50 >&2
  fi
  printf '\n===== host network =====\n' >&2
  sudo ip -details address show >&2
  sudo ip route show table all >&2
  sudo ip neigh show >&2
  sudo bridge link show >&2
  sudo bridge fdb show >&2
  printf '\n===== host firewall =====\n' >&2
  sudo nft list ruleset >&2
  sudo iptables-save >&2
  printf '\n===== QEMU processes =====\n' >&2
  pgrep -a -f qemu-system >&2
  if [[ "$daemon_ready" == true && -x "$root/bin/tbx" ]]; then
    while read -r node; do
      [[ -n "$node" ]] || continue
      printf '\n===== console %s/%s =====\n' "$diagnostics_cluster" "$node" >&2
      timeout 3s "$root/bin/tbx" console "$diagnostics_cluster" "$node" </dev/null >&2
    done < <(HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" status "$diagnostics_cluster" --quiet -o json | jq -r '.[0].nodes[].name')
  fi
  printf '\n===== end diagnostics =====\n' >&2
)
cleanup() {
  if [[ "$cluster_cleanup_needed" == true && -x "$root/bin/tbx" && -n "$home" && -n "$helper_socket" ]]; then
    HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" cluster destroy e2e-storage --force || true
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

if [[ -n ${TBX_E2E_WORKDIR:-} ]]; then
  scratch_parent=$(dirname "$TBX_E2E_WORKDIR")
  usable_scratch "$scratch_parent" || {
    printf 'TBX_E2E_WORKDIR parent %s is not suitable scratch\n' "$scratch_parent" >&2
    exit 1
  }
  workdir=$TBX_E2E_WORKDIR
  [[ ! -e "$workdir" ]] || {
    printf 'refusing to reuse existing TBX_E2E_WORKDIR %s\n' "$workdir" >&2
    exit 1
  }
  mkdir "$workdir"
else
  workdir=$(mktemp -d "$(select_scratch)/talosbox-kvm-storage-e2e.XXXXXX")
fi
touch "$workdir/.talosbox-e2e-owned"
workdir_owned=true

home="$workdir/home"
helper_socket="$workdir/tbx-helper.sock"
tools="$workdir/tools"

mkdir -p "$home" "$tools"
export HOME="$home"
export PATH="$tools:$PATH"
export TBX_HELPER_SOCKET="$helper_socket"

for binary in tbx tbxd tbx-helper; do
  test -x "$root/bin/$binary"
done

kubectl_version=v1.34.1
kubectl_sha256=7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3
curl --fail --location --retry 3 --output "$tools/kubectl" \
  "https://dl.k8s.io/release/${kubectl_version}/bin/linux/amd64/kubectl"
printf '%s  %s\n' "$kubectl_sha256" "$tools/kubectl" | sha256sum --check --strict -
chmod +x "$tools/kubectl"

# The log intentionally remains user-owned so a failed privileged helper is inspectable.
# shellcheck disable=SC2024
sudo env HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx-helper" --allowed-uid "$(id -u)" \
  >"$workdir/tbx-helper.log" 2>&1 &
helper_pid=$!
retry "helper socket" 30 1 socket_ready "$helper_socket"

"$root/bin/tbxd" >"$workdir/tbxd.log" 2>&1 &
daemon_pid=$!
wait_for_process_socket "tbxd" 30 1 "$daemon_pid" "$home/.talosbox/tbxd.sock" "$workdir/tbxd.log"

kubeconfig="$home/.talosbox/clusters/e2e-storage/kubeconfig"
kc() {
  kubectl --kubeconfig "$kubeconfig" "$@"
}

storage_live() {
  [[ "$("$root/bin/tbx" status e2e-storage --quiet -o json | jq -r '.[0].storagePhase')" == live ]]
}

assert_default_storage_class() {
  local expected=$1 actual
  actual=$(kc get storageclass -o json |
    jq -r '.items[] | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true") | .metadata.name')
  if [[ "$actual" != "$expected" ]]; then
    printf 'default StorageClass = %q, want %q\n' "$actual" "$expected" >&2
    return 1
  fi
}

pod_succeeded() {
  [[ "$(kc get pod "$1" -n storage-e2e -o jsonpath='{.status.phase}' 2>/dev/null)" == Succeeded ]]
}

# The PVC deliberately names no StorageClass: charts that assume a cluster
# default must work unmodified. The writer runs to completion before the reader
# attaches so the readback proves data persisted across pods.
verify_pvc_write_readback() {
  kc apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: storage-e2e
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: probe
  namespace: storage-e2e
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
  namespace: storage-e2e
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: docker.io/library/busybox:1.37.0
      command:
        - sh
        - -c
        - printf 'talosbox-storage-e2e' > /data/probe && sync
      volumeMounts:
        - name: storage
          mountPath: /data
  volumes:
    - name: storage
      persistentVolumeClaim:
        claimName: probe
EOF
  retry "writer pod completion" 120 5 pod_succeeded writer
  [[ "$(kc get pvc probe -n storage-e2e -o jsonpath='{.status.phase}')" == Bound ]]
  kc delete pod writer -n storage-e2e --wait
  kc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: reader
  namespace: storage-e2e
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: docker.io/library/busybox:1.37.0
      command:
        - sh
        - -c
        - test "$(cat /data/probe)" = talosbox-storage-e2e
      volumeMounts:
        - name: storage
          mountPath: /data
  volumes:
    - name: storage
      persistentVolumeClaim:
        claimName: probe
EOF
  retry "reader pod readback" 120 5 pod_succeeded reader
}

# Replicas derive from node count capped at 3, so this 3-node cluster must
# replicate the probe volume exactly 3 times.
assert_longhorn_replicas() {
  local replica_counts
  replica_counts=$(kc -n longhorn-system get volumes.longhorn.io -o json |
    jq -r '[.items[].spec.numberOfReplicas] | unique | join(",")')
  if [[ "$replica_counts" != "3" ]]; then
    printf 'longhorn volume numberOfReplicas = %q, want exactly "3"\n' "$replica_counts" >&2
    return 1
  fi
}

longhorn_config="$workdir/talosbox-longhorn.yaml"
cat >"$longhorn_config" <<'EOF'
version: 1
clusters:
  - name: e2e-storage
    controlPlanes: 1
    workers: 2
    cni: flannel
    csi: longhorn
    node:
      diskSize: 10GiB
    controlPlane:
      memory: 2GiB
      cpus: 2
    worker:
      memory: 2GiB
      cpus: 1
EOF
cluster_cleanup_needed=true
retry "longhorn cluster provisioning" 3 10 "$root/bin/tbx" up -f "$longhorn_config" --force --quiet
retry "longhorn storage live" 60 5 storage_live
assert_default_storage_class longhorn
verify_pvc_write_readback
assert_longhorn_replicas
kc delete namespace storage-e2e --wait
"$root/bin/tbx" cluster destroy e2e-storage --force

# The single-node shape proves the lightweight engine and that a worker-less
# control plane schedules workloads at all.
localpath_config="$workdir/talosbox-local-path.yaml"
cat >"$localpath_config" <<'EOF'
version: 1
clusters:
  - name: e2e-storage
    controlPlanes: 1
    workers: 0
    cni: flannel
    csi: local-path
    node:
      diskSize: 10GiB
    controlPlane:
      memory: 3GiB
      cpus: 2
EOF
retry "local-path cluster provisioning" 3 10 "$root/bin/tbx" up -f "$localpath_config" --force --quiet
retry "local-path storage live" 60 5 storage_live
assert_default_storage_class local-path
verify_pvc_write_readback
kc delete namespace storage-e2e --wait
"$root/bin/tbx" cluster destroy e2e-storage --force
cluster_cleanup_needed=false

printf 'verified longhorn (3-node, 3 replicas) and local-path (single-node) PVC write/readback through the provisioning path\n'
