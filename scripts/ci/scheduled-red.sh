#!/usr/bin/env bash
# A scheduled run on main, red or green, keeps one open issue per workflow in step:
# red opens it (or comments on it), green closes it. Hub ci-result mail skips
# schedule events, so without this a red nightly reaches nobody. The text is ours;
# nothing here reads a PR or a log.
# Env: GH_TOKEN, GH_REPO, WORKFLOW (title prefix), RED (true|false), RUN (run URL),
# CLOSE (true: a green run may close the issue; default true).
set -euo pipefail
: "${GH_REPO:?}" "${WORKFLOW:?}" "${RED:?}" "${RUN:?}"
title="$WORKFLOW: scheduled run red on main"
open=$(gh issue list --state open --search "\"$title\" in:title" --json number,title \
	--jq ".[] | select(.title == \"$title\") | .number" | head -1)
if [ "$RED" = true ]; then
	body="The scheduled $WORKFLOW run on main (${GITHUB_SHA:-?}) failed: $RUN"
	if [ -n "$open" ]; then gh issue comment "$open" --body "$body"; else gh issue create --title "$title" --body "$body"; fi
elif [ -n "$open" ] && [ "${CLOSE:-true}" = true ]; then
	gh issue close "$open" --comment "Green again: $RUN"
fi
