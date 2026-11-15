#!/usr/bin/env bash
# Fetches one pinned source and fails unless it matches its pin.
# Usage: fetch.sh git <repo-url> <tag> <commit> <dir>
#        fetch.sh tar <url> <sha256> <dir> [strip-components]
#        fetch.sh file <url> <sha256> <path>
set -euo pipefail

case $1 in
git)
	git clone -q --depth 1 -b "$3" "$2" "$5"
	commit=$(git -C "$5" rev-parse HEAD)
	if [[ $commit != "$4" ]]; then
		echo "$2 tag $3 is commit $commit, want $4" >&2
		exit 1
	fi
	;;
tar)
	curl -fsSL -o /tmp/fetch.tar.gz "$2"
	echo "$3  /tmp/fetch.tar.gz" | sha256sum -c -
	mkdir -p "$4"
	tar -xzf /tmp/fetch.tar.gz -C "$4" --strip-components="${5:-0}"
	rm /tmp/fetch.tar.gz
	;;
file)
	curl -fsSL -o "$4" "$2"
	echo "$3  $4" | sha256sum -c -
	;;
*)
	echo "usage: fetch.sh git|tar|file ..." >&2
	exit 2
	;;
esac
