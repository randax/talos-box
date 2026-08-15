# shellcheck shell=bash
# Shared plumbing for the Linux KVM e2e harnesses. Sourced, never executed;
# callers set $root always and $required_bytes before using the scratch helpers.
# shellcheck disable=SC2154

available_bytes() {
  df -Pk "$1" | awk 'NR == 2 { print $4 * 1024 }'
}

usable_scratch() {
  local path=$1 probe available
  [[ -d "$path" && -w "$path" ]] || return 1
  probe=$(mktemp "$path/talosbox-e2e-probe.XXXXXX") || return 1
  rm -f "$probe"
  available=$(available_bytes "$path")
  [[ ${available:-0} -ge $required_bytes ]]
}

select_scratch() {
  local candidate best="" best_available=0 available
  for candidate in "${RUNNER_TEMP:-}" /mnt /tmp "$root"; do
    [[ -n "$candidate" ]] || continue
    if usable_scratch "$candidate"; then
      available=$(available_bytes "$candidate")
      if [[ $available -gt $best_available ]]; then
        best=$candidate
        best_available=$available
      fi
    fi
  done
  [[ -n "$best" ]] || {
    printf 'no writable scratch mount has the required %d bytes\n' "$required_bytes" >&2
    return 1
  }
  printf '%s\n' "$best"
}

retry() {
  local label=$1 attempts=$2 delay=$3 attempt
  shift 3
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if "$@"; then
      return 0
    fi
    if ((attempt < attempts)); then
      sleep "$delay"
    fi
  done
  printf 'timed out waiting for %s after %d attempts\n' "$label" "$attempts" >&2
  return 1
}

socket_ready() {
  [[ -S "$1" ]]
}

wait_for_process_socket() {
  local label=$1 attempts=$2 delay=$3 pid=$4 socket=$5 log_file=$6 attempt status
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if socket_ready "$socket"; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      status=0
      wait "$pid" || status=$?
      printf '%s exited with status %d before creating %s\n' "$label" "$status" "$socket" >&2
      cat "$log_file" >&2
      return 1
    fi
    if ((attempt < attempts)); then
      sleep "$delay"
    fi
  done
  printf 'timed out waiting for %s after %d attempts\n' "$label" "$attempts" >&2
  cat "$log_file" >&2
  return 1
}
