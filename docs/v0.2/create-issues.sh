#!/usr/bin/env bash
# Turns docs/v0.2/milestones/*.md into GitHub milestones and issues: one milestone per file ("# M1 · Title"),
# one issue per ticket ("### M1-02 · Title" with its body). Tickets whose Status starts with "done" are skipped,
# and so is any issue whose title already exists. Dry run by default; --apply creates. Needs gh (gh auth login).
set -euo pipefail
cd "$(dirname "$0")/milestones"
apply=false
[ "${1:-}" = "--apply" ] && apply=true
repo=${OMEGACD_REPO:-obleey/omegacd}

if $apply; then
	command -v gh >/dev/null || { echo "gh not found" >&2; exit 1; }
	gh auth status >/dev/null 2>&1 || { echo "run: gh auth login" >&2; exit 1; }
	gh label create v0.2 --repo "$repo" --color 8839ef --description "v0.2: pull-based Agents" 2>/dev/null || true
	existing=$(gh issue list --repo "$repo" --state all --limit 1000 --json title --jq '.[].title')
	milestones=$(gh api "repos/$repo/milestones?state=all&per_page=100" --jq '.[].title')
else
	existing="" milestones=""
fi

for f in M*.md; do
	milestone=$(sed -n '1s/^# //p' "$f")
	if $apply && ! grep -Fxq "$milestone" <<<"$milestones"; then
		gh api "repos/$repo/milestones" -f title="$milestone" -f description="See docs/v0.2/milestones/$f" >/dev/null
		echo "milestone: $milestone"
	elif ! $apply; then
		echo "milestone: $milestone"
	fi
	# split the file into tickets: a "### " heading up to the next one
	awk -v out="$(mktemp -d)" '
		/^### / { if (file) close(file); n++; file = out "/" sprintf("%02d", n); print > file; next }
		file { print >> file }
		END { print out }
	' "$f" | tail -1 | while read -r dir; do
		for t in "$dir"/*; do
			title=$(sed -n '1s/^### //p' "$t" | sed 's/ · / /')
			status=$(sed -n 's/^\*\*Status:\*\* //p' "$t")
			case $status in done*) echo "  skip (done): $title"; continue ;; esac
			if grep -Fxq "$title" <<<"$existing"; then echo "  skip (exists): $title"; continue; fi
			body=$(sed '1d' "$t"; printf '\n---\nFrom [docs/v0.2/milestones/%s](../blob/main/docs/v0.2/milestones/%s); update its Status line in the PR that closes this.\n' "$f" "$f")
			if $apply; then
				gh issue create --repo "$repo" --title "$title" --body "$body" --label v0.2 --milestone "$milestone" >/dev/null
				echo "  created: $title"
			else
				echo "  would create: $title"
			fi
		done
		rm -rf "$dir"
	done
done
