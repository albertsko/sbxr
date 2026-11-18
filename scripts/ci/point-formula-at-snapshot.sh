#!/usr/bin/env bash
# Points the tapped Homebrew formula albertsko/tap/sbxr at the darwin arm64 archive of the goreleaser snapshot in dist/.
# Usage: scripts/ci/point-formula-at-snapshot.sh (after brew taps the formula)
# A file URL carries no version, so the formula gets the snapshot version for brew test.
# Later steps must skip the auto-update, which would reset the edited formula, so this sets HOMEBREW_NO_AUTO_UPDATE for them.
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."

archive=$PWD/dist/sbxr_darwin_arm64.tar.gz
formula=$(brew --repo albertsko/tap)/Formula/sbxr.rb
sha=$(shasum -a 256 "$archive" | cut -d ' ' -f 1)
version=$(jq -r .version dist/metadata.json)

sed -i '' \
	-e "s|^  url \".*\"|  url \"file://$archive\"\\
  version \"$version\"|" \
	-e "s|^  sha256 \".*\"|  sha256 \"$sha\"|" \
	"$formula"
cat "$formula"
if ! grep -qF "file://$archive" "$formula"; then
	echo "$formula has no url line to replace" >&2
	exit 1
fi

echo HOMEBREW_NO_AUTO_UPDATE=1 >>"$GITHUB_ENV"
