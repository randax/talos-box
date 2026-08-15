#!/usr/bin/env bash
# Exercises the public substrate-only workflow from VM creation through
# externally managed Talos configuration, bootstrap, Flannel, and Ready nodes.
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

required_bytes=$((14 * 1024 * 1024 * 1024))

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
    for node in e2e-cp-1 e2e-worker-1 e2e-worker-2; do
      printf '\n===== console e2e/%s =====\n' "$node" >&2
      timeout 3s "$root/bin/tbx" console e2e "$node" </dev/null >&2
    done
  fi
  printf '\n===== end diagnostics =====\n' >&2
)
cleanup() {
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
  workdir=$(mktemp -d "$(select_scratch)/talosbox-kvm-e2e.XXXXXX")
fi
touch "$workdir/.talosbox-e2e-owned"
workdir_owned=true

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

talos_version=v1.13.6
kubectl_version=v1.34.1
talosctl_sha256=540c5e7cb0d3fa3a9b2e1c717ced212727b73bcaf0cf9cf9ba2472ec381041d4
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
# a modest hosted-runner footprint and sparse 10 GiB raw disks.
cat >"$cluster_config" <<'EOF'
version: 1
clusters:
  - name: e2e
    controlPlanes: 1
    workers: 2
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

maintenance_api_ready() {
  talosctl version --nodes "$1" --insecure >/dev/null 2>&1
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
printf 'verified 1 control plane + 2 workers through substrate-only path to Ready nodes\n'
