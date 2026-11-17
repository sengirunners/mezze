#!/usr/bin/env bash
# Put rg on PATH for the search brick's tests (SPEC 21), without apt: some pool
# runners cannot resolve archive.ubuntu.com, but every runner reaches
# GitHub. Fetches a pinned ripgrep release (retried, sha256-checked) into the
# runner's tool cache once and adds that directory to $GITHUB_PATH.
# Env: RG_DIR overrides the install directory.
set -euo pipefail

if command -v rg >/dev/null 2>&1; then exec rg --version; fi

version=14.1.1
case "$(uname -m)" in
x86_64 | amd64)
	target=x86_64-unknown-linux-musl
	sum=4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e
	;;
aarch64 | arm64)
	target=aarch64-unknown-linux-gnu
	sum=c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f
	;;
*)
	echo "::error::no pinned ripgrep build for $(uname -m)" >&2
	exit 1
	;;
esac
name=ripgrep-$version-$target
dir=${RG_DIR:-${RUNNER_TOOL_CACHE:-$HOME/.cache}/ripgrep/$version-$target}

if [ ! -x "$dir/rg" ]; then
	tmp=$(mktemp -d)
	trap 'rm -rf -- "$tmp"' EXIT
	# --retry-all-errors: a DNS miss (curl exit 6) is not "transient" to plain --retry.
	curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 120 \
		-o "$tmp/$name.tar.gz" "https://github.com/BurntSushi/ripgrep/releases/download/$version/$name.tar.gz"
	printf '%s  %s\n' "$sum" "$tmp/$name.tar.gz" | sha256sum -c --strict -
	tar -xzf "$tmp/$name.tar.gz" -C "$tmp" "$name/rg"
	mkdir -p "$dir"
	# Same directory, then rename: a half-copied rg is never at the cached path.
	cp "$tmp/$name/rg" "$dir/rg.partial"
	chmod +x "$dir/rg.partial"
	mv -f "$dir/rg.partial" "$dir/rg"
fi

"$dir/rg" --version
if [ -n "${GITHUB_PATH:-}" ]; then echo "$dir" >>"$GITHUB_PATH"; fi
