#!/usr/bin/env bash
# Exercises the curated provisioning path with storage end to end: flannel +
# longhorn on a multinode cluster (including replica behavior), then flannel +
# local-path on a single node. PVC bind and write/readback are verified with
# kubectl, independently of tbx's own storage probe.
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

# Two sequential clusters plus the mirror cache holding the curated storage images.
required_bytes=$((20 * 1024 * 1024 * 1024))

# The cluster is named "e2es" and the workdir template kept short deliberately:
# the daemon binds per-node Unix sockets under
# $HOME/.talosbox/clusters/<name>/<node>.console.sock, and Linux rejects socket
# paths over 108 bytes with "bind: invalid argument".

workdir=""
workdir_owned=false
home=""
helper_socket=""
helper_pid=""
daemon_pid=""
cluster_cleanup_needed=false

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
    HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" status e2es --quiet -o json >&2
  fi
  kubeconfig="$home/.talosbox/clusters/e2es/kubeconfig"
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
  # The helper's DHCP sockets bind with SO_BINDTODEVICE, so a listener on a
  # bridge that no longer exists is invisible except here (ip address show
  # prints each bridge's current ifindex above).
  sudo ss -ulpn 'sport = :67' >&2
  printf '\n===== host firewall =====\n' >&2
  sudo nft list ruleset >&2
  sudo iptables-save >&2
  printf '\n===== QEMU processes =====\n' >&2
  pgrep -a -f qemu-system >&2
  if [[ "$daemon_ready" == true && -x "$root/bin/tbx" ]]; then
    while read -r node; do
      [[ -n "$node" ]] || continue
      printf '\n===== console %s/%s =====\n' e2es "$node" >&2
      timeout 3s "$root/bin/tbx" console e2es "$node" </dev/null >&2
    done < <(HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" status e2es --quiet -o json | jq -r '.[0].nodes[].name')
  fi
  printf '\n===== end diagnostics =====\n' >&2
)
cleanup() {
  if [[ "$cluster_cleanup_needed" == true && -x "$root/bin/tbx" && -n "$home" && -n "$helper_socket" ]]; then
    HOME="$home" TBX_HELPER_SOCKET="$helper_socket" "$root/bin/tbx" cluster destroy e2es --force || true
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

prepare_workdir "tbx-storage.XXXXXX"

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

kubeconfig="$home/.talosbox/clusters/e2es/kubeconfig"
kc() {
  kubectl --kubeconfig "$kubeconfig" "$@"
}

storage_live() {
  [[ "$("$root/bin/tbx" status e2es --quiet -o json | jq -r '.[0].storagePhase')" == live ]]
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

node_name_by_role() {
  "$root/bin/tbx" status e2es --quiet -o json |
    jq -r --arg role "$1" 'first(.[0].nodes[] | select(.role == $role) | .name) // empty'
}

ready_node_count() {
  [[ "$(kc get nodes -o json |
    jq -r '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length')" -eq "$1" ]]
}

# Crossing the zero-worker boundary flips two control-plane settings that Talos
# re-asserts from machine config: the NoSchedule taint and the load-balancer
# exclusion label. A worker-less cluster drops both so workloads can schedule
# at all; the first worker must restore them, and losing it must drop them
# again. Both are asserted together because tbx sets them in lockstep.
control_plane_reserved() {
  local want=$1 node actual
  node=$(node_name_by_role control-plane) || return 1
  [[ -n "$node" ]] || return 1
  actual=$(kc get node "$node" -o json 2>/dev/null | jq -r '
    [((.spec.taints // []) | any(.key == "node-role.kubernetes.io/control-plane" and .effect == "NoSchedule")),
     (.metadata.labels | has("node.kubernetes.io/exclude-from-external-load-balancers"))] |
    map(tostring) | join(",")') || return 1
  [[ "$actual" == "$want,$want" ]]
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

# Replicas derive from the nodes that can host them (workers, capped at 3), so
# this 1cp+2w cluster must replicate the probe volume exactly twice — and the
# volume must actually reach healthy robustness, proving every replica scheduled.
assert_longhorn_replicas() {
  local replica_counts
  replica_counts=$(kc -n longhorn-system get volumes.longhorn.io -o json |
    jq -r '[.items[].spec.numberOfReplicas] | unique | join(",")')
  if [[ "$replica_counts" != "2" ]]; then
    printf 'longhorn volume numberOfReplicas = %q, want exactly "2"\n' "$replica_counts" >&2
    return 1
  fi
}

# Replica CRs record their scheduled node in spec.nodeID and survive volume
# detach (robustness reads "unknown" once the workload pod terminates), so this
# proves both replicas actually landed on distinct nodes without racing the
# writer pod's lifetime. longhorn-manager tolerates the control-plane taint, so
# "2 distinct nodes" alone would also pass with a replica beside etcd: the
# control plane must hold none while the cluster has workers.
longhorn_replicas_scheduled() {
  local control_plane nodes
  control_plane=$(node_name_by_role control-plane) || return 1
  [[ -n "$control_plane" ]] || return 1
  nodes=$(kc -n longhorn-system get replicas.longhorn.io -o json |
    jq -r '[.items[].spec.nodeID | select(. != null and . != "")] | unique')
  [[ "$(jq -r 'length' <<<"$nodes")" -eq 2 ]] || return 1
  [[ "$(jq -r --arg cp "$control_plane" 'index($cp) // "none"' <<<"$nodes")" == none ]]
}

# A control plane is reserved in one of two ways: longhorn-manager never runs
# there, so no node resource exists to schedule onto; or the manager does run
# there (a worker-less phase left replicas) and the node resource says no.
# Anything else — a present node that allows scheduling, or an API error — is
# not reserved.
longhorn_control_plane_unschedulable() {
  local node allow
  node=$(node_name_by_role control-plane) || return 1
  [[ -n "$node" ]] || return 1
  if ! allow=$(kc -n longhorn-system get nodes.longhorn.io "$node" -o jsonpath='{.spec.allowScheduling}' 2>&1); then
    [[ "$allow" == *"not found"* ]]
    return
  fi
  [[ "$allow" == false ]]
}

longhorn_config="$workdir/talosbox-longhorn.yaml"
cat >"$longhorn_config" <<'EOF'
version: 1
clusters:
  - name: e2es
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
retry "longhorn cluster provisioning" 2 10 "$root/bin/tbx" up -f "$longhorn_config" --force --quiet
retry "longhorn storage live" 60 5 storage_live
assert_default_storage_class longhorn
verify_pvc_write_readback
assert_longhorn_replicas
retry "longhorn control plane unschedulable" 60 5 longhorn_control_plane_unschedulable
retry "longhorn replicas scheduled on 2 workers" 60 5 longhorn_replicas_scheduled
kc delete namespace storage-e2e --wait
"$root/bin/tbx" cluster destroy e2es --force

# The single-node shape proves the lightweight engine and that a worker-less
# control plane schedules workloads at all.
localpath_config="$workdir/talosbox-local-path.yaml"
cat >"$localpath_config" <<'EOF'
version: 1
clusters:
  - name: e2es
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
retry "local-path cluster provisioning" 2 10 "$root/bin/tbx" up -f "$localpath_config" --force --quiet
retry "local-path storage live" 60 5 storage_live
assert_default_storage_class local-path
verify_pvc_write_readback
kc delete namespace storage-e2e --wait

# Both crossings of the zero-worker boundary must reconcile the already
# configured control plane, not just freshly generated machine configs.
retry "worker-less control plane schedulable" 12 5 control_plane_reserved false
"$root/bin/tbx" node add e2es --role worker --force
retry "worker node registration" 120 5 ready_node_count 2
retry "storage live after node add" 120 5 storage_live
retry "control plane reserved after node add" 120 5 control_plane_reserved true

added_worker=$(node_name_by_role worker)
[[ -n "$added_worker" ]]
"$root/bin/tbx" node remove e2es "$added_worker" --force
retry "worker node deregistration" 120 5 ready_node_count 1
retry "storage live after node remove" 120 5 storage_live
retry "control plane schedulable after node remove" 120 5 control_plane_reserved false

"$root/bin/tbx" cluster destroy e2es --force
cluster_cleanup_needed=false

printf 'verified longhorn (1cp+2w, 2 replicas) and local-path (single-node) PVC write/readback through the provisioning path, and both crossings of the zero-worker boundary\n'
