#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
makefile="$root/Makefile"

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

require() {
  local needle=$1
  local output=$2
  local reason=$3
  if ! grep -Fq -- "$needle" <<< "$output"; then
    fail "$reason"
  fi
}

require_build_commands() {
  local output=$1
  local context=$2
  local command

  for command in '-o ./bin/tbx ./cmd/tbx' '-o ./bin/tbxd ./cmd/tbxd' '-o ./bin/tbx-helper ./cmd/tbx-helper'; do
    if ! awk -v command="$command" 'index($0, "go build ") && index($0, command) { found = 1 } END { exit !found }' <<< "$output"; then
      fail "$context must contain a go build command matching: $command"
    fi
  done
}

require_build_commands_once() {
  local output=$1
  local context=$2
  local command count

  for command in '-o ./bin/tbx ./cmd/tbx' '-o ./bin/tbxd ./cmd/tbxd' '-o ./bin/tbx-helper ./cmd/tbx-helper'; do
    count=$(awk -v command="$command" 'index($0, "go build ") && index($0, command) { count++ } END { print count + 0 }' <<< "$output")
    if [[ "$count" -ne 1 ]]; then
      fail "$context must contain exactly one build command matching: $command"
    fi
  done
}

if ! darwin_e2e=$(UNAME_S=Darwin VERSION=contract make -C "$root" -n e2e); then
  fail 'Darwin e2e dry run failed'
fi
if ! linux_e2e=$(UNAME_S=Linux VERSION=contract make -C "$root" -n e2e); then
  fail 'Linux e2e dry run failed'
fi

require_build_commands "$darwin_e2e" 'Darwin e2e'
require 'codesign' "$darwin_e2e" 'Darwin e2e must run the signed build prerequisite'
require 'codesign --force --sign - --entitlements ./build/entitlements.plist ./bin/tbxd' "$darwin_e2e" 'Darwin e2e must codesign tbxd'
require 'codesign --force --sign - --entitlements ./build/entitlements.plist ./bin/tbx' "$darwin_e2e" 'Darwin e2e must codesign tbx'
require_build_commands "$linux_e2e" 'Linux e2e'
if grep -Fq -- 'codesign' <<< "$linux_e2e"; then
  fail 'Linux e2e must build binaries without codesign'
fi

for branch_output in "$darwin_e2e" "$linux_e2e"; do
  require '-count=1' "$branch_output" 'e2e dry run must disable the Go test cache with -count=1'
  require '-timeout 90m' "$branch_output" 'e2e dry run must set the Go test timeout to 90m'
done

if ! linux_qemu_e2e=$(TBX_E2E_HYPERVISOR=qemu UNAME_S=Linux VERSION=contract make -C "$root" -n e2e); then
  fail 'Linux QEMU e2e dry run failed'
fi
require 'TBX_E2E_HYPERVISOR=qemu' "$linux_qemu_e2e" 'e2e must preserve a caller-provided hypervisor on the Go test invocation'

check_e2e_all() {
  local uname_s=$1
  local output test_lines test_count first_test second_test

  if ! output=$(UNAME_S="$uname_s" VERSION=contract make -C "$root" -n e2e-all); then
    fail "$uname_s e2e-all dry run failed"
  fi

  require_build_commands_once "$output" "$uname_s e2e-all"
  if grep -Eq -- '(^|[[:space:]])(\$\(MAKE\)|([^[:space:]/]*/)?make)[[:space:]]+e2e([[:space:]]|$)' <<< "$output"; then
    fail "$uname_s e2e-all must not recursively invoke the e2e target"
  fi

  test_lines=$(awk 'index($0, "-tags e2e") { print }' <<< "$output")
  test_count=$(awk 'index($0, "-tags e2e") { count++ } END { print count + 0 }' <<< "$output")
  if [[ "$test_count" -ne 2 ]]; then
    fail "$uname_s e2e-all must contain exactly two Go e2e test invocations"
  fi

  first_test=$(sed -n '1p' <<< "$test_lines")
  second_test=$(sed -n '2p' <<< "$test_lines")
  require 'TBX_E2E_HYPERVISOR=vz' "$first_test" "$uname_s e2e-all must run the VZ lane first"
  require 'TBX_E2E_HYPERVISOR=qemu' "$second_test" "$uname_s e2e-all must run the QEMU lane second"
}

check_e2e_all Linux
check_e2e_all Darwin

if ! grep -Eq -- '\.PHONY:.*\be2e-all\b' "$makefile"; then
  fail 'Makefile .PHONY membership must include e2e-all as a distinct target'
fi
