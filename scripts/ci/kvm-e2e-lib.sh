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

# prepare_workdir honors TBX_E2E_WORKDIR (never reusing an existing path) or
# picks the best scratch mount, then marks the directory as harness-owned.
# Sets the caller's $workdir and $workdir_owned.
prepare_workdir() {
  local template=$1 scratch_parent
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
    workdir=$(mktemp -d "$(select_scratch)/$template")
  fi
  touch "$workdir/.talosbox-e2e-owned"
  # shellcheck disable=SC2034 # read by the callers' cleanup traps
  workdir_owned=true
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
