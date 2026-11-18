#!/usr/bin/env bash
# Usage: scripts/ci/check-require.sh
# On a Linux runner without KVM, checks that the vm and kube tasks skip their tests, and fail with SBXR_TEST_REQUIRE=1.
set -euo pipefail

fail() {
	printf 'check-require: %s\n' "$1" >&2
	exit 1
}

if [[ "$(uname -s)" != Linux ]]; then
	fail "this check needs Linux, but the host is $(uname -s)"
fi

if [[ -e /dev/kvm ]]; then
	fail "KVM is unexpectedly available (/dev/kvm exists), so the vm and kube tests would run instead of skip"
fi

unset SBXR_TEST_REQUIRE

for target in vm kube; do
	task="test:$target"
	prefix=TestVM
	if [[ "$target" == kube ]]; then
		prefix=TestKube
	fi

	printf '::group::mise run %s (must skip)\n' "$task"
	if ! out="$(mise run "$task" -v 2>&1)"; then
		printf '%s\n' "$out"
		printf '::endgroup::\n'
		fail "mise run $task failed, but without KVM it must skip and succeed"
	fi
	printf '%s\n' "$out"
	printf '::endgroup::\n'

	if ! grep -q -- "--- SKIP: $prefix" <<<"$out"; then
		fail "mise run $task succeeded, but skipped no $prefix test, so it selected none"
	fi

	printf '::group::SBXR_TEST_REQUIRE=1 mise run %s (must fail)\n' "$task"
	# The failure is the expected result, so it must not stop the script.
	status=0
	out="$(SBXR_TEST_REQUIRE=1 mise run "$task" -v 2>&1)" || status=$?
	printf '%s\n' "$out"
	printf '::endgroup::\n'

	if [[ "$status" -eq 0 ]]; then
		fail "SBXR_TEST_REQUIRE=1 mise run $task succeeded, but without KVM it must fail"
	fi
	if ! grep -q "SBXR_TEST_REQUIRE=1 requires it" <<<"$out"; then
		fail "SBXR_TEST_REQUIRE=1 mise run $task failed (exit $status), but not because a $prefix test required its environment"
	fi

	printf 'check-require: %s skips without KVM and fails with SBXR_TEST_REQUIRE=1 (exit %d), as expected\n' "$task" "$status"
done
