#!/bin/sh
# Verify the artifacts attached to a release.
#
# Usage: verify-artifacts.sh <dist-dir> <version>
#
# Checking the checksums is not enough on its own. `sha256sum --check
# --ignore-missing` passes an archive that has no line in checksums.txt at
# all, so a truncated checksums file would verify "successfully" while
# saying nothing about most of the release. Each expected archive is
# therefore required to appear exactly once in checksums.txt before the
# checksums are checked at all, and the check then runs without
# --ignore-missing so a listed-but-absent file fails too.
set -eu

dist=${1:?usage: verify-artifacts.sh <dist-dir> <version>}
version=${2:?usage: verify-artifacts.sh <dist-dir> <version>}
cd "$dist"

expected="ofpack_linux_amd64.tar.gz ofpack_linux_arm64.tar.gz ofpack_darwin_amd64.tar.gz ofpack_darwin_arm64.tar.gz"
failed=0

if [ ! -f checksums.txt ]; then
  echo "::error::checksums.txt is missing"
  exit 1
fi

for f in $expected; do
  if [ ! -f "$f" ]; then
    echo "::error::missing artifact $f"
    failed=$((failed + 1))
    continue
  fi
  # sha256sum writes "<digest>  <name>". Compared as a string, not a
  # pattern: a file name is full of dots, and as a regular expression each
  # one matches any character, so a near-miss name would count as a match.
  lines=$(awk -v want="$f" '$2 == want { n++ } END { print n+0 }' checksums.txt)
  if [ "$lines" != "1" ]; then
    echo "::error::checksums.txt has $lines lines for $f, expected exactly 1"
    failed=$((failed + 1))
    continue
  fi
  echo "ok   $f is present and checksummed"
done

if [ "$failed" -ne 0 ]; then
  echo "::error::$failed artifact problem(s); not checking digests"
  exit 1
fi

# No --ignore-missing: everything listed has to be here and match.
sha256sum --check checksums.txt

tar -xzf ofpack_linux_amd64.tar.gz ofpack
got=$(./ofpack version | awk '{print $2}')
if [ "$got" != "$version" ]; then
  echo "::error::the released binary reports $got, but the release is $version"
  exit 1
fi
echo "ofpack $got matches the release"
