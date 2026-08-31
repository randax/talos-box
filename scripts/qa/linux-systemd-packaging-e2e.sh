#!/usr/bin/env bash
# Exercises the checked-in Linux package as installed: both systemd managers,
# the privileged helper's live sandbox, persistent DHCP state, routed traffic,
# helper recovery, and supervised daemon restart refusal. This intentionally
# installs files under /usr and is only safe on a disposable Linux host.
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
opt_in=--i-know-this-installs-system-files
cluster_a=qa-sd-a
cluster_b=qa-sd-b
state_file=/var/lib/tbx/reservations.json
state_backup=""
workdir=""
workdir_owned=false
state_backup_moved_flag=""
cleanup_needed=false

# shellcheck source=scripts/ci/kvm-e2e-lib.sh disable=SC1091
source "$root/scripts/ci/kvm-e2e-lib.sh"

usage() {
  printf 'usage: %s %s\n' "$0" "$opt_in" >&2
}

refuse() {
  printf 'refusing packaged-systemd QA: %s\n' "$1" >&2
  exit 1
}

dump_failure_diagnostics() (
  trap - ERR
  set +e
  printf '\n===== talosbox packaged-systemd QA failure diagnostics =====\n' >&2
  printf '\n===== system units =====\n' >&2
  sudo systemctl status tbx-helper.socket tbx-helper.service --no-pager -l >&2
  systemctl --user status tbxd.socket tbxd.service --no-pager -l >&2
  printf '\n===== system journal =====\n' >&2
  sudo journalctl -u tbx-helper.socket -u tbx-helper.service --no-pager -n 300 >&2
  printf '\n===== user journal =====\n' >&2
  journalctl --user -u tbxd.socket -u tbxd.service --no-pager -n 300 >&2
  printf '\n===== sockets and processes =====\n' >&2
  sudo ss -ulpn 'sport = :67' >&2
  sudo ss -xlpn >&2
  pgrep -a -f 'tbxd|tbx-helper|qemu-system' >&2
  printf '\n===== host network =====\n' >&2
  sudo ip -details address show >&2
  sudo ip route show table all >&2
  sudo ip neigh show >&2
  sudo bridge link show >&2
  sudo bridge fdb show >&2
  printf '\n===== host firewall =====\n' >&2
  sudo nft list ruleset >&2
  sudo iptables-save >&2
  printf '\n===== forwarding =====\n' >&2
  sysctl net.ipv4.ip_forward >&2
  for forwarding in /proc/sys/net/ipv4/conf/br-tbx*/forwarding; do
    [[ -e "$forwarding" ]] && printf '%s=%s\n' "$forwarding" "$(<"$forwarding")" >&2
  done
  printf '\n===== helper state =====\n' >&2
  sudo stat "$state_file" >&2
  sudo sed -n '1,240p' "$state_file" >&2
  printf '\n===== cluster status =====\n' >&2
  /usr/bin/tbx cluster list -o json >&2
  /usr/bin/tbx status "$cluster_a" --quiet -o json >&2
  /usr/bin/tbx status "$cluster_b" --quiet -o json >&2
  printf '\n===== daemon log =====\n' >&2
  if [[ -f "$HOME/.talosbox/tbxd.log" ]]; then
    tail -n 300 "$HOME/.talosbox/tbxd.log" >&2
  fi
  printf '\n===== end diagnostics =====\n' >&2
)

cleanup() {
  if [[ "$cleanup_needed" == true && -x /usr/bin/tbx ]]; then
    /usr/bin/tbx cluster destroy "$cluster_b" --force 2>/dev/null || true
    /usr/bin/tbx cluster destroy "$cluster_a" --force 2>/dev/null || true
  fi
  if [[ -n "$state_backup_moved_flag" && -f "$state_backup_moved_flag" ]]; then
    if ! restore_state_backup; then
      printf 'FAILED to restore %s from %s\n' "$state_file" "$state_backup" >&2
      return 1
    fi
  fi
  if [[ "$workdir_owned" == true && -f "$workdir/.talosbox-e2e-owned" ]]; then
    rm -rf -- "$workdir"
  fi
}
trap cleanup EXIT

restore_state_backup() {
  sudo test -e "$state_backup"
  sudo rm -f -- "$state_file"
  sudo mv "$state_backup" "$state_file"
}

delete_state_backup() {
  sudo test -e "$state_backup"
  sudo rm -f -- "$state_backup"
  rm -f -- "$state_backup_moved_flag"
}

[[ $# -eq 1 && $1 == "$opt_in" ]] || {
  usage
  refuse "pass $opt_in only on a disposable host"
}
[[ "$(uname -s)" == Linux ]] || refuse 'Linux is required'
case "$(uname -m)" in
  x86_64) qemu_system_bin=qemu-system-x86_64 ;;
  aarch64) qemu_system_bin=qemu-system-aarch64 ;;
  *) refuse "unsupported architecture $(uname -m): need qemu-system-x86_64 or qemu-system-aarch64" ;;
esac
[[ "$(readlink /proc/1/exe)" == */systemd ]] || refuse 'PID 1 is not systemd'
system_state=$(systemctl is-system-running 2>/dev/null || true)
[[ "$system_state" == running || "$system_state" == degraded ]] ||
  refuse "systemd state is $system_state, want running or degraded"
test -w /dev/kvm || refuse '/dev/kvm is not writable'

for group in tbx kvm; do
  id -nG | tr ' ' '\n' | grep -Fxq "$group" || refuse "caller lacks active $group group membership"
done
for command in awk bridge cmp curl find getconf go ip jq make nft pgrep "$qemu_system_bin" readlink ss stat sudo systemctl systemd-sysusers; do
  command -v "$command" >/dev/null || refuse "required command is missing: $command"
done
[[ $(getconf _NPROCESSORS_ONLN) -ge 4 ]] || refuse 'at least 4 logical CPUs are required'
memory_kib=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
[[ ${memory_kib:-0} -ge $((8 * 1024 * 1024)) ]] || refuse 'at least 8 GiB available memory is required'
required_bytes=$((40 * 1024 * 1024 * 1024))
[[ $(available_bytes "$root") -ge $required_bytes ]] || refuse 'at least 40 GiB free disk is required'
prepare_workdir "tbx-systemd-packaging-e2e.XXXXXX"
state_backup="$workdir/reservations.json.backup"
state_backup_moved_flag="$workdir/reservations-backup.moved"
[[ -d /sys/class/net/lo ]] || refuse 'the host network namespace is unavailable'
[[ -c /dev/net/tun ]] || refuse '/dev/net/tun is required'
sudo true || refuse 'sudo authorization is required'
if sudo ss -H -ulpn 'sport = :67' | grep -q .; then
  refuse 'UDP port 67 already has a listener'
fi
if sudo nft list table inet tbx >/dev/null 2>&1; then
  refuse 'table inet tbx already exists'
fi
if sudo test -e "$state_file"; then
  refuse "$state_file already exists; restore or remove it before running this harness"
fi
if sudo test -e "$state_backup"; then
  refuse "$state_backup already exists; restore or remove the stale backup before running this harness"
fi

# Do not take over an existing user installation. Exact-name conflicts and any
# other configured cluster both indicate that this is not the disposable host
# the operator opted into using.
if [[ -d "$HOME/.talosbox/clusters" ]] && find "$HOME/.talosbox/clusters" -mindepth 1 -maxdepth 1 -type d -print -quit | grep -q .; then
  refuse 'configured clusters already exist under ~/.talosbox/clusters'
fi

trap dump_failure_diagnostics ERR

make -C "$root" binaries
for binary in tbx tbxd tbx-helper; do
  test -x "$root/bin/$binary"
  sudo install -m 0755 "$root/bin/$binary" "/usr/bin/$binary"
done

# Install the packaging tree verbatim before asking either manager to load it.
sudo cp -a "$root/packaging/linux/." /
sudo systemd-sysusers /usr/lib/sysusers.d/talos-box.conf
sudo systemctl daemon-reload
systemctl --user daemon-reload
sudo systemctl enable --now tbx-helper.socket
systemctl --user enable --now tbxd.socket
sudo systemctl start tbx-helper.service
/usr/bin/tbx cluster list -o json >/dev/null
retry 'helper service activation' 30 1 sudo systemctl is-active --quiet tbx-helper.service
retry 'daemon service activation' 30 1 systemctl --user is-active --quiet tbxd.service

for binary in tbx tbxd tbx-helper; do
  cmp "$root/bin/$binary" "/usr/bin/$binary"
done
for unit in \
  usr/lib/systemd/system/tbx-helper.service \
  usr/lib/systemd/system/tbx-helper.socket \
  usr/lib/systemd/user/tbxd.service \
  usr/lib/systemd/user/tbxd.socket; do
  cmp "$root/packaging/linux/$unit" "/$unit"
done

[[ "$(systemctl show tbx-helper.service -p User --value)" == tbx ]]
[[ "$(systemctl show tbx-helper.service -p Group --value)" == tbx ]]
[[ "$(systemctl show tbx-helper.service -p StateDirectory --value)" == tbx ]]
[[ "$(systemctl show tbx-helper.service -p StateDirectoryMode --value)" == 0700 ]]
[[ "$(systemctl show tbx-helper.service -p AmbientCapabilities --value)" == 'cap_net_bind_service cap_net_admin cap_net_raw' ]]
[[ "$(systemctl show tbx-helper.service -p CapabilityBoundingSet --value)" == 'cap_net_bind_service cap_net_admin cap_net_raw' ]]
helper_pid=$(systemctl show tbx-helper.service -p MainPID --value)
[[ "$helper_pid" =~ ^[1-9][0-9]*$ ]]
[[ "$(readlink "/proc/$helper_pid/exe")" == /usr/bin/tbx-helper ]]
[[ "$(awk '/^CapEff:/ {print $2}' "/proc/$helper_pid/status")" == 0000000000003400 ]]
[[ "$(awk '/^CapBnd:/ {print $2}' "/proc/$helper_pid/status")" == 0000000000003400 ]]
daemon_pid=$(systemctl --user show tbxd.service -p MainPID --value)
[[ "$daemon_pid" =~ ^[1-9][0-9]*$ ]]
[[ "$(readlink "/proc/$daemon_pid/exe")" == /usr/bin/tbxd ]]
[[ "$(stat -c '%U:%G %a' /var/lib/tbx)" == 'tbx:tbx 700' ]]
[[ "$(stat -Lc '%U:%G %a' /var/run/tbx-helper.sock)" == 'tbx:tbx 660' ]]
[[ "$(stat -Lc '%U:%G %a' "$HOME/.talosbox/tbxd.sock")" == "$(id -un):$(id -gn) 600" ]]

status_json() {
  /usr/bin/tbx status "$1" --quiet -o json
}

cluster_field() {
  status_json "$1" | jq -er ".[0].$2"
}

bridge_for_cluster() {
  local subnet index
  subnet=$(cluster_field "$1" subnet)
  index=$(awk -F. '{print $3}' <<<"$subnet")
  printf 'br-tbx%s\n' "$index"
}

dhcp_listener_on() {
  sudo ss -H -ulpn 'sport = :67' | grep -Fq "%$1:67"
}

reservation_has_cluster() {
  sudo jq -e --arg cluster "$1" '[.owners[][] | select(.name == $cluster)] | length == 1' "$state_file" >/dev/null
}

assert_cluster_host_contract() {
  local cluster=$1 bridge subnet node_name node_mac node_ip nft_state
  bridge=$(bridge_for_cluster "$cluster")
  subnet=$(cluster_field "$cluster" subnet)
  node_name=$(status_json "$cluster" | jq -er '.[0].nodes[0].name')
  node_mac=$(status_json "$cluster" | jq -er '.[0].nodes[0].mac')
  node_ip=$(status_json "$cluster" | jq -er '.[0].nodes[0].ip')
  [[ "$(< /proc/sys/net/ipv4/ip_forward)" == 1 ]]
  [[ "$(< "/proc/sys/net/ipv4/conf/$bridge/forwarding")" == 1 ]]
  [[ "$(sudo stat -c '%U:%G %a' "$state_file")" == 'tbx:tbx 600' ]]
  sudo jq -e --arg cluster "$cluster" --arg node "$node_name" --arg mac "$node_mac" --arg ip "$node_ip" '
    any(.owners[][]; .name == $cluster and any(.nodes[]; .name == $node and .mac == $mac and .ip == $ip))
  ' "$state_file" >/dev/null
  dhcp_listener_on "$bridge"
  nft_state=$(sudo nft list table inet tbx)
  grep -Fq "$bridge" <<<"$nft_state"
  grep -Fq "$subnet" <<<"$nft_state"
  status_json "$cluster" | jq -e --arg ip "$node_ip" '.[0].nodes[0].ip == $ip and .[0].nodes[0].apidReachable' >/dev/null
}

create_cluster() {
  /usr/bin/tbx cluster create "$1" --cp 1 --workers 0 --memory-mib 2048 --cpus 2 --disk-gib 10 --cni flannel --force --quiet
}

cleanup_needed=true
create_cluster "$cluster_a"
assert_cluster_host_contract "$cluster_a"
create_cluster "$cluster_b"
assert_cluster_host_contract "$cluster_b"

vip_a=$(cluster_field "$cluster_a" vip)
vip_b=$(cluster_field "$cluster_b" vip)
[[ -n "$vip_a" && -n "$vip_b" ]]
curl --fail --silent --show-error --max-time 10 "http://$vip_a/" >/dev/null
curl --fail --silent --show-error --max-time 10 "http://$vip_b/" >/dev/null

assert_routed_dial() {
  local source=$1 target=$2 answer
  answer=$(curl --fail --silent --show-error --max-time 25 --get \
    --data-urlencode "host=$target" \
    --data-urlencode 'port=80' \
    --data-urlencode 'request=hostname' \
    --data-urlencode 'protocol=http' \
    --data-urlencode 'tries=1' \
    "http://$source/dial")
  jq -e '(.errors | length) == 0 and (.responses | length) > 0' <<<"$answer" >/dev/null
}
assert_routed_dial "$vip_a" "$vip_b"
assert_routed_dial "$vip_b" "$vip_a"
doctor_output=$(/usr/bin/tbx doctor 2>&1 || true)
grep -Fq 'PASS inter-cluster: 2 cluster VIP(s) reachable from the host and from each sibling' <<<"$doctor_output"

# First prove an ordinary packaged restart. Then prove #514 by removing all
# persisted desired state and waiting through the one-minute net.sync cadence.
sudo systemctl restart tbx-helper.service
retry 'helper after normal service restart' 30 1 sudo systemctl is-active --quiet tbx-helper.service
retry 'first DHCP listener after normal restart' 30 1 dhcp_listener_on "$(bridge_for_cluster "$cluster_a")"
retry 'second DHCP listener after normal restart' 30 1 dhcp_listener_on "$(bridge_for_cluster "$cluster_b")"

sudo systemctl stop tbx-helper.service
sudo test -e "$state_file"
sudo mv "$state_file" "$state_backup"
touch "$state_backup_moved_flag"
sudo systemctl start tbx-helper.service

periodic_sync_recovered() {
  sudo test -e "$state_file" || return 1
  reservation_has_cluster "$cluster_a" || return 1
  reservation_has_cluster "$cluster_b" || return 1
  dhcp_listener_on "$(bridge_for_cluster "$cluster_a")" || return 1
  dhcp_listener_on "$(bridge_for_cluster "$cluster_b")"
}
retry 'periodic net.sync reservation and DHCP recovery' 19 5 periodic_sync_recovered
delete_state_backup

node_a=$(status_json "$cluster_a" | jq -er '.[0].nodes[0].name')
reserved_ip_a=$(status_json "$cluster_a" | jq -er '.[0].nodes[0].ip')
/usr/bin/tbx node stop "$cluster_a" "$node_a"
retry 'node stopped' 30 1 sh -c "/usr/bin/tbx status '$cluster_a' --quiet -o json | jq -e '.[0].nodes[0].phase == \"stopped\"' >/dev/null"
/usr/bin/tbx node start "$cluster_a" "$node_a"
node_reacquired_reservation() {
  status_json "$cluster_a" | jq -e --arg ip "$reserved_ip_a" \
    '.[0].nodes[0].ip == $ip and .[0].nodes[0].apidReachable' >/dev/null
}
retry 'fresh DHCP exchange and maintenance reachability' 90 2 node_reacquired_reservation

daemon_pid_before=$(systemctl --user show tbxd.service -p MainPID --value)
restart_output=""
restart_status=0
restart_output=$(/usr/bin/tbx system restart 2>&1) || restart_status=$?
[[ $restart_status -ne 0 ]]
grep -Fq "$cluster_a" <<<"$restart_output"
grep -Fq "$cluster_b" <<<"$restart_output"
grep -Fq 'systemctl --user restart tbxd.service' <<<"$restart_output"
if grep -Fq -- '--force' <<<"$restart_output"; then
  printf 'supervised restart refusal incorrectly suggested --force:\n%s\n' "$restart_output" >&2
  exit 1
fi
clusters_offset=${restart_output%%systemctl --user restart tbxd.service*}
grep -Fq "$cluster_a" <<<"$clusters_offset"
grep -Fq "$cluster_b" <<<"$clusters_offset"
[[ "$(systemctl --user show tbxd.service -p MainPID --value)" == "$daemon_pid_before" ]]
systemctl --user is-active --quiet tbxd.service

bridge_a=$(bridge_for_cluster "$cluster_a")
bridge_b=$(bridge_for_cluster "$cluster_b")
subnet_a=$(cluster_field "$cluster_a" subnet)
subnet_b=$(cluster_field "$cluster_b" subnet)
/usr/bin/tbx cluster destroy "$cluster_b" --force
/usr/bin/tbx cluster destroy "$cluster_a" --force
cleanup_needed=false

if ip link show "$bridge_a" >/dev/null 2>&1 || ip link show "$bridge_b" >/dev/null 2>&1; then
  printf 'destroy left a test bridge behind\n' >&2
  exit 1
fi
if dhcp_listener_on "$bridge_a" || dhcp_listener_on "$bridge_b"; then
  printf 'destroy left a test DHCP listener behind\n' >&2
  exit 1
fi
if reservation_has_cluster "$cluster_a" || reservation_has_cluster "$cluster_b"; then
  printf 'destroy left a test reservation behind\n' >&2
  exit 1
fi
nft_after=$(sudo nft list table inet tbx 2>/dev/null || true)
if grep -Fq "$bridge_a" <<<"$nft_after" || grep -Fq "$bridge_b" <<<"$nft_after" ||
  grep -Fq "$subnet_a" <<<"$nft_after" || grep -Fq "$subnet_b" <<<"$nft_after"; then
  printf 'destroy left test nftables entries behind\n' >&2
  exit 1
fi

printf 'packaged-systemd QA passed for %s and %s\n' "$cluster_a" "$cluster_b"
