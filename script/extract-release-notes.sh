#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
#
# Extract the changelog.md section for the given version and emit it
# as a self-contained release-notes payload on stdout. Single source
# of truth shared by `make release-notes` (local dry-run) and the
# .github/workflows/release-on-tag.yml workflow.
#
# Uses only POSIX-compatible awk + sed so it runs the same on macOS
# (BSD awk) and Ubuntu (gawk) — no `match($0, regex, array)` tricks.
#
# Usage: script/extract-release-notes.sh <version>
#
# Exits non-zero when no matching section is found, so `make release`
# fails fast instead of producing an empty release body.

set -euo pipefail

if [ $# -lt 1 ]; then
	echo "usage: $0 <version>" >&2
	exit 2
fi

VERSION="$1"
CHANGELOG="${CHANGELOG:-changelog.md}"

if [ ! -f "$CHANGELOG" ]; then
	echo "error: $CHANGELOG not found at $(pwd)" >&2
	exit 1
fi

# Body: skip the header line itself, print everything until the next
# "# Version " header (or EOF).
body=$(awk -v ver="$VERSION" '
	/^# Version / {
		if (insec) exit
		if ($0 ~ "^# Version " ver " ") { insec=1; next }
	}
	insec { print }
' "$CHANGELOG")

if [ -z "$body" ]; then
	echo "error: no '# Version $VERSION ' section found in $CHANGELOG" >&2
	exit 1
fi

# Previous version: the next "# Version <tag> ..." header that appears
# after our section. Splitting the regex/extraction into awk+sed keeps
# us off the gawk-only match-with-array form.
prev_header=$(awk -v ver="$VERSION" '
	$0 ~ "^# Version " ver " " { insec=1; next }
	insec && /^# Version / { print; exit }
' "$CHANGELOG")

prev_version=""
if [ -n "$prev_header" ]; then
	prev_version=$(printf '%s\n' "$prev_header" | sed -E 's/^# Version ([^ ]+).*$/\1/')
fi

repo="${GITHUB_REPOSITORY:-SukramJ/go-mtec2mqtt}"

# Assemble the body, then optionally the compare link. The first
# release has no predecessor — that's fine, just skip the link. The
# refs are v-prefixed because that is what the tags are actually
# named (v1.9.0, not 1.9.0), while the changelog headers are bare.
notes=$(mktemp)
trap 'rm -f "$notes"' EXIT

printf '%s\n' "$body" > "$notes"

if [ -n "$prev_version" ]; then
	printf '\n**Full Changelog**: https://github.com/%s/compare/v%s...v%s\n' \
		"$repo" "$prev_version" "$VERSION" >> "$notes"
fi

# GitHub caps a release body at 125,000 characters and answers 422
# when it is exceeded. That failure lands *after* the tag is already
# on the remote, so it cannot be undone by re-running the job — hence
# a hard budget here rather than a check at the call site. The budget
# is in bytes (LC_ALL=C makes awk's length() count bytes), which for
# UTF-8 is always >= the character count GitHub measures, so this
# errs on the safe side. Truncation happens on a line boundary, never
# mid-multi-byte-sequence.
MAX_BYTES="${RELEASE_NOTES_MAX_BYTES:-120000}"

size=$(wc -c < "$notes" | tr -d ' ')

if [ "$size" -le "$MAX_BYTES" ]; then
	cat "$notes"
else
	echo "warning: release notes for $VERSION are $size bytes, over the" \
		"$MAX_BYTES budget — truncating to stay under GitHub's" \
		"125,000-character release-body limit" >&2
	LC_ALL=C awk -v budget="$MAX_BYTES" '
		{
			n = length($0) + 1
			if (total + n > budget) exit
			total += n
			print
		}
	' "$notes"
	printf '\n---\n\nThese release notes were truncated to fit GitHub'"'"'s release-body limit. The complete section is in [changelog.md](https://github.com/%s/blob/v%s/changelog.md).\n' \
		"$repo" "$VERSION"
fi
