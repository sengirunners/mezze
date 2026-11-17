GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# The GitHub repository (owner/name) of this checkout's origin: zm reads its releases after the public repo's
# (cmd/zm buildRepo). Only owner/name leaves this line (never a URL's credentials); any other origin gives none.
BUILD_REPO := $(shell git remote get-url origin 2>/dev/null | sed -E 's,/+$$,,; s,\.git$$,,; s,^([a-z+]+://)?([^@/]*@)?github\.com(:[0-9]+)?[:/],,' | grep -E '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$$')
LDFLAGS := -X github.com/sengirunners/potluck/kit/api.Version=$(VERSION) -X github.com/sengirunners/potluck/kit/api.Commit=$(COMMIT) -X main.buildRepo=$(BUILD_REPO)

# The run-target drivers live in sengirunners/potluck, a public
# module. Built next to zm at the version go.mod requires.
# Each driver ships under its Potluck name and its old one, one program: the
# daemon looks for potluck-driver-<name> first, then zm-run-<name> (and the
# host probe for potluck-conformance, then zm-run-conformance).
POTLUCK_CMDS := $(addprefix github.com/sengirunners/potluck/cmd/,potluck-driver-tart potluck-driver-local potluck-conformance zm-run-tart zm-run-local zm-run-conformance zm-e2b)

# Tests run with a throwaway HOME and ZM_HOME, so nothing can touch real dotfiles
# (~/.claude, ~/.codex, ~/.zm) and pass silently. The Go caches are kept.
GOENV_KEEP := GOCACHE=$$($(GO) env GOCACHE) GOMODCACHE=$$($(GO) env GOMODCACHE) GOPATH=$$($(GO) env GOPATH)

.PHONY: build test vet check bench loadtest install release clean

build:
	GOWORK=off $(GO) build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/... $(POTLUCK_CMDS)

test:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	env $(GOENV_KEEP) HOME="$$tmp/home" ZM_HOME="$$tmp/zm" $(GO) test -p 2 -timeout 20m ./... # internal/team runs ~8 of go test's default 10 min on a CI VM

vet:
	$(GO) vet ./...

check: vet test
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt needed"; exit 1)

# Latency ceilings (scripts/ceilings/ceilings.json: p95, process start included)
# against a real daemon in a temp ZM_HOME, native issues and a replica on a
# generated 11.5k-issue store; fails when one is over. N=100 for more runs.
bench:
	@scripts/bench.sh

# SPEC 17.7's load test over the loopback tailnet: an authority and 5 replicas
# x 8 agents for 10 minutes (DURATION=40s for a quick one), each daemon in a
# throwaway ZM_HOME; fails on a lost or duplicated write, a double claim, an
# outbox that does not drain, a copy that does not converge or a p95 over its
# ceiling (the CI VMs' numbers: BUDGETS=false anywhere else).
loadtest:
	@$(GO) run ./scripts/loadtest -duration $(or $(DURATION),10m) $(if $(OUT),-out $(OUT)) $(if $(BUDGETS),-budgets=$(BUDGETS))

# Installs through `zm daemon upgrade` (SPEC 2, "Swaps"): build into a temp
# dir, run that build's preflight against this home's daemon (refused: the
# reason, and nothing is installed), place every file in INSTALL_DIR keeping
# each previous one as <name>.prev, restart the daemon when it runs from
# there, verify it serves what it served, and roll back when it does not.
# FORCE=1 overrides the preflight (loud, recorded); END_SESSIONS=1 lets
# managed sessions end. Never `go install` over a running daemon's binary.
# Runbook: docs/runbooks/zm-upgrade.md (systemd and launchd: restarted through them).
#
# What it installs: what a newcomer needs, and nothing else.
#   zm                    the CLI and the daemon
#   bd-zm                 bd's daily verbs over zm-native issues (named bd-zm so it never
#                         shadows a Beads bd; alias it to bd if you like)
#   potluck-driver-tart   lends this machine as VM boxes (zm host share, zm pool host)
#   potluck-driver-local  runs jobs on this machine itself (a run target of kind local)
#   potluck-conformance   the probe zm host share and zm pool host run against a driver
# EXTRAS=1 adds the rest (an extra already in INSTALL_DIR is always refreshed,
# so an older install never keeps a stale one):
#   zm-run-tart zm-run-local zm-run-conformance   the drivers' old names
#   zm-e2b                E2B cloud sandboxes as a run target
#   zm-run-farm           team machines as run targets over zm exec
#   zm-connect            the bridge to the outreach Worker (deploy/cloudflare/zm-connect)
#   zm-work-conformance zm-agent-conformance   checks for adapter authors
INSTALL_DIR ?= $(or $(shell $(GO) env GOBIN),$(firstword $(subst :, ,$(shell $(GO) env GOPATH)))/bin)
INSTALL_CORE := ./cmd/zm ./cmd/bd-zm $(addprefix github.com/sengirunners/potluck/cmd/,potluck-driver-tart potluck-driver-local potluck-conformance)
INSTALL_EXTRAS := ./cmd/zm-run-farm ./cmd/zm-connect ./cmd/zm-work-conformance ./cmd/zm-agent-conformance \
	$(addprefix github.com/sengirunners/potluck/cmd/,zm-run-tart zm-run-local zm-run-conformance zm-e2b)
install:
	@stage=$$(mktemp -d); trap 'rm -rf "$$stage"' EXIT; \
	pkgs="$(INSTALL_CORE)"; \
	for p in $(INSTALL_EXTRAS); do \
		if [ "$(EXTRAS)" = 1 ] || [ -e "$(INSTALL_DIR)/$${p##*/}" ]; then pkgs="$$pkgs $$p"; fi; \
	done; \
	GOWORK=off $(GO) build -ldflags "$(LDFLAGS)" -o "$$stage/" $$pkgs && \
	echo "installing: $$(cd "$$stage" && ls | tr '\n' ' ')" && \
	"$$stage/zm" daemon upgrade "$$stage/zm" --dir "$(INSTALL_DIR)" $(if $(FORCE),--force) $(if $(END_SESSIONS),--end-sessions)

# Release tarballs (no publishing): dist/zm_<version>_<os>_<arch>.tar.gz for
# darwin/linux x arm64/amd64, static (CGO_ENABLED=0), plus dist/SHA256SUMS
# (shasum -a 256 -c SHA256SUMS). TARGETS=darwin/arm64 builds one.
release:
	$(GO) run ./scripts/release -version $(VERSION) -commit $(COMMIT) $(if $(BUILD_REPO),-repo '$(BUILD_REPO)') -out dist $(if $(TARGETS),-targets $(TARGETS))

clean:
	rm -rf bin dist
