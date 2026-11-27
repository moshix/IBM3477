#!/bin/bash
# Publish everything in bin/ (from ./build.bash) as a GitHub release of
# this repository, tagged with the version (e.g. 0.9.6, as earlier
# releases), at the current commit.
#
# usage: ./release.bash [-n] [-y] [-t TITLE] [-F NOTES-FILE]
#   -n             dry run: check and list what would be uploaded, publish nothing
#   -y             do not ask for confirmation
#   -t TITLE       release title (default: "IBM 3477 <version>")
#   -F NOTES-FILE  release notes from a file (default: GitHub's generated
#                  notes, the commits since the last release)
#
# Files are uploaded as they are, except app bundles (folders such as
# bin/IBM3477.app): they are packed with ditto into
# <name>-<version>-macos-universal.zip first, which keeps the bundle's
# code signature and is what Finder unpacks with a double-click.
#
# Needs the GitHub CLI (gh), logged in (gh auth login).
set -euo pipefail
cd "$(dirname "$0")"

dry=0 yes=0 title="" notes=""
while getopts "nyt:F:" opt; do
  case $opt in
    n) dry=1 ;;
    y) yes=1 ;;
    t) title=$OPTARG ;;
    F) notes=$OPTARG ;;
    *) sed -n '5,10p' "$0" >&2; exit 2 ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }

version=$(sed -n 's/^const Number = "\(.*\)"$/\1/p' internal/version/version.go)
[ -n "$version" ] || die "no version in internal/version/version.go"
title=${title:-IBM 3477 $version}
[ -z "$notes" ] || [ -f "$notes" ] || die "notes file $notes not found"

# --- checks: everything that would make a wrong release ------------------------
command -v gh >/dev/null || die "gh (GitHub CLI) not found"
gh auth status >/dev/null 2>&1 || die "gh is not logged in (gh auth login)"
[ -d bin ] && [ -n "$(ls -A bin)" ] || die "bin/ is empty; run ./build.bash"
# bin/ must come from this version: build.bash names files with it
ls bin/*-"$version"-* >/dev/null 2>&1 ||
  die "bin/ has no files of version $version; run ./build.bash"
for f in bin/*; do
  case $f in *-[0-9]*.[0-9]*.[0-9]*-*)
    case $f in *-"$version"-*) ;; *) die "$f is not version $version; run ./build.bash" ;; esac ;;
  esac
done
# the release points at a commit: it must be committed and pushed
[ -z "$(git status --porcelain --untracked-files=no)" ] ||
  die "uncommitted changes; commit them first"
commit=$(git rev-parse HEAD)
git fetch -q origin
[ -n "$(git branch -r --contains "$commit")" ] ||
  die "commit ${commit:0:7} is not pushed; git push first"
if gh release view "$version" >/dev/null 2>&1; then
  die "release $version exists already; raise the version in internal/version/version.go"
fi

# --- assets ------------------------------------------------------------------
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
assets=()
for f in bin/*; do
  name=$(basename "$f")
  case $name in .*) continue ;; esac # .DS_Store and the like
  if [ -d "$f" ]; then
    case $name in
      *.app)
        zip="$stage/${name%.app}-$version-macos-universal.zip"
        ditto -c -k --keepParent "$f" "$zip"
        assets+=("$zip") ;;
      *) echo "skipping folder $f" >&2 ;;
    esac
  else
    assets+=("$f")
  fi
done

echo "Release $version \"$title\" of $(gh repo view --json nameWithOwner -q .nameWithOwner)"
echo "at commit ${commit:0:7} ($(git log -1 --format=%s))"
echo "Files:"
for a in "${assets[@]}"; do
  printf '  %-50s %6s KB\n' "$(basename "$a")" $(( $(stat -f%z "$a") / 1024 ))
done
if [ $dry = 1 ]; then
  echo "Dry run: nothing published."
  exit 0
fi
if [ $yes = 0 ]; then
  read -r -p "Publish this release? [y/N] " ok
  case $ok in y|Y|yes) ;; *) echo "Not published."; exit 1 ;; esac
fi

# gh creates the tag on GitHub at $commit
if [ -n "$notes" ]; then
  gh release create "$version" --target "$commit" --title "$title" -F "$notes" "${assets[@]}"
else
  gh release create "$version" --target "$commit" --title "$title" --generate-notes "${assets[@]}"
fi
git fetch -q --tags origin # the new tag, locally too
