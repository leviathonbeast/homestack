#!/usr/bin/env bash
# Homestack's guard rails. The git hooks in .githooks run this for you; turn
# them on once per clone with `make hooks` (or `git config core.hooksPath .githooks`).
#
#   scripts/check.sh          everything: also the tests, known vulnerabilities,
#                             and whether every dependency is on its latest
#                             stable release. Needs the network. Runs before
#                             every push.
#   scripts/check.sh --quick  the offline checks, for what's staged. Runs before
#                             every commit.
#
# A check that can't run counts as a failure: a guard that quietly skips
# isn't a guard.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

quick=0
[ "${1:-}" = --quick ] && quick=1

if [ -t 1 ]; then red=$'\e[31m' green=$'\e[32m' bold=$'\e[1m' off=$'\e[0m'; else red='' green='' bold='' off=''; fi
failed=()
section() { printf '\n%s%s%s\n' "$bold" "$1" "$off"; }
pass() { printf '  %sok%s    %s\n' "$green" "$off" "$1"; }
fail() { printf '  %sFAIL%s  %s\n' "$red" "$off" "$1"; failed+=("$1"); }
# check <label> <command...>: run it, and show its output only if it fails.
check() {
  local label=$1 out
  shift
  if out=$("$@" 2>&1); then pass "$label"; else
    fail "$label"
    [ -z "$out" ] || printf '%s\n' "$out" | sed 's/^/        /'
  fi
}
# newer A B: version A is B or later.
newer() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$2" ]; }
need() { command -v "$1" >/dev/null || { echo "$1 isn't installed. $2"; return 1; }; }

# What to look at: the staged files (--quick) or everything.
if ((quick)); then files=$(git diff --cached --name-only --diff-filter=ACMR); else files=$(git ls-files); fi
touches() { grep -qE "$1" <<<"$files"; }

TOOLS=${HOMESTACK_TOOLS:-${XDG_CACHE_HOME:-$HOME/.cache}/homestack-tools}
# tool <name> <package>: a Go tool built for this machine, on its latest
# release. --quick reuses one built earlier with this repo's Go, so it works
# offline. It's built with the latest golang.org/x/tools, not the one the tool
# was released with: a Go point release can change the compiler's export data
# before the tool's next release catches up (Go 1.27.2 did; staticcheck 0.8.1
# couldn't read it).
tool() {
  local bin=$TOOLS/$1 go_version built mod rc
  [ -x "$bin" ] || bin=$bin.exe
  go_version=$(go env GOVERSION) || return 1
  go_version=${go_version%% *}
  built=$(go version "$bin" 2>/dev/null)
  if ((quick)) && [ "${built##*: }" = "$go_version" ]; then echo "$bin"; return; fi
  mkdir -p "$TOOLS" && mod=$(mktemp -d) || return 1
  (
    cd "$mod" || exit 1
    export GOTOOLCHAIN=$go_version
    go mod init tools >/dev/null 2>&1 &&
      go get "$2@latest" golang.org/x/tools@latest &&
      GOOS=$(go env GOHOSTOS) GOARCH=$(go env GOHOSTARCH) go build -o "$TOOLS/" "$2"
  ) >&2
  rc=$?
  rm -rf "$mod"
  ((rc == 0)) && echo "$TOOLS/$1"
}
# The code is Linux-only, so analyse it as Linux on any machine.
linux() { GOOS=linux GOARCH=amd64 "$@"; }

# ---------- Checks ----------

line_endings() {
  local bad
  bad=$(git ls-files --eol | grep -E '(^| )(i|w)/(crlf|mixed)' | cut -f2-)
  [ -z "$bad" ] && return 0
  echo "Windows (CRLF) line endings break the scripts on Linux. Convert these to LF (VS Code: click CRLF in the status bar):"
  echo "$bad"
  return 1
}

gofmt_check() {
  local bad
  bad=$(git ls-files -z '*.go' | xargs -0 gofmt -l) || return 1
  [ -z "$bad" ] || { echo "Run gofmt -w on:"; echo "$bad"; return 1; }
}
staticcheck_check() { local t; t=$(tool staticcheck honnef.co/go/tools/cmd/staticcheck) && linux "$t" ./...; }
actionlint_check() { local t; t=$(tool actionlint github.com/rhysd/actionlint/cmd/actionlint) && "$t" -no-color; }
govulncheck_check() { local t; t=$(tool govulncheck golang.org/x/vuln/cmd/govulncheck) && linux "$t" ./...; }

go_tests() {
  if [ "$(go env GOHOSTOS)" = linux ]; then go test ./...; return; fi
  # Elsewhere, in the Go image the Dockerfile builds with.
  local img src
  img=$(sed -nE 's/^FROM (--platform=[^ ]+ )?(golang:[^ ]+).*/\2/p' Dockerfile | head -1)
  docker info >/dev/null 2>&1 || { echo "The tests need Linux: start Docker Desktop and try again."; return 1; }
  src=$(pwd -W 2>/dev/null || pwd)
  MSYS_NO_PATHCONV=1 docker run --rm -v "$src:/src" -w /src \
    -v homestack-gomod:/go/pkg/mod -v homestack-gocache:/root/.cache/go-build "$img" go test ./...
}

go_modules_latest() {
  local out
  out=$(go list -m -u -f '{{if and (not .Main) .Update}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all) || return 1
  out=$(sed '/^$/d' <<<"$out")
  [ -z "$out" ] || { echo "Update with: go get <module>@latest && go mod tidy"; echo "$out"; return 1; }
}

go_toolchain_latest() {
  local latest img here rc=0
  latest=$(curl -fsS 'https://go.dev/VERSION?m=text' | head -1) || { echo "Couldn't reach go.dev."; return 1; }
  latest=${latest#go}
  img=$(sed -nE 's/^FROM (--platform=[^ ]+ )?golang:([0-9.]+)-.*/\2/p' Dockerfile)
  here=$(go env GOVERSION)
  here=${here#go}
  here=${here%% *}
  [ "$img" = "$latest" ] || { echo "The Dockerfile builds with Go $img; the latest is $latest."; rc=1; }
  newer "$here" "$latest" || { echo "This machine has Go $here; the latest is $latest."; rc=1; }
  return $rc
}

node_latest() {
  local latest img here rc=0
  latest=$(node -e 'fetch("https://nodejs.org/dist/index.json").then((r) => r.json())
    .then((v) => console.log(v.find((x) => x.lts).version.slice(1))).catch(() => process.exit(1))') ||
    { echo "Couldn't reach nodejs.org."; return 1; }
  img=$(sed -nE 's/^FROM (--platform=[^ ]+ )?node:([0-9]+)-.*/\2/p' Dockerfile)
  here=$(node -p process.versions.node)
  [ "$img" = "${latest%%.*}" ] || { echo "The Dockerfile builds the UI with Node $img; the latest LTS is Node ${latest%%.*}."; rc=1; }
  newer "$here" "$latest" || { echo "This machine has Node $here; the latest LTS is $latest."; rc=1; }
  return $rc
}

npm_latest() {
  local latest img here rc=0
  latest=$(npm view npm dist-tags.latest) || { echo "Couldn't reach the npm registry."; return 1; }
  img=$(sed -nE 's/.*npm install -g npm@([0-9.]+).*/\1/p' Dockerfile)
  here=$(npm --version)
  [ "$img" = "$latest" ] || { echo "The Dockerfile installs npm ${img:-(the one bundled with Node)}; the latest is $latest."; rc=1; }
  newer "$here" "$latest" || { echo "This machine has npm $here; the latest is $latest (npm install -g npm@latest)."; rc=1; }
  return $rc
}

alpine_latest() {
  local latest old
  latest=$(curl -fsS https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/x86_64/latest-releases.yaml |
    sed -nE 's/^ *version: *([0-9]+\.[0-9]+).*/\1/p' | head -1)
  [ -n "$latest" ] || { echo "Couldn't reach alpinelinux.org."; return 1; }
  old=$(grep -oE 'alpine:?[0-9]+\.[0-9]+' Dockerfile | sed -E 's/alpine:?//' | sort -u | grep -vxF "$latest")
  [ -z "$old" ] || { echo "The Dockerfile uses Alpine $old; the latest is $latest."; return 1; }
}

web() { (cd web && "$@"); }
web_deps() { [ -d web/node_modules ] && ((quick)) && return 0; web npm ci --no-audit --no-fund; }
web_build() {
  local out rc
  out=$(mktemp -d)
  web npx vite build --outDir "$out" --emptyOutDir --logLevel error
  rc=$?
  rm -rf "$out"
  return $rc
}

shell_scripts() {
  git ls-files -z deploy scripts .githooks '*.sh' |
    xargs -0 awk 'FNR == 1 { if (/^#!.*[\/ ](ba)?sh( |$)/) print FILENAME; nextfile }'
}
shellcheck_check() {
  need shellcheck "Install it: winget install koalaman.shellcheck (Windows) or apt install shellcheck (Debian)." || return 1
  local list
  mapfile -t list < <(shell_scripts)
  shellcheck "${list[@]}"
}

# ---------- Run ----------

if ((quick)); then
  printf '%sQuick checks (offline) of what you are committing%s\n' "$bold" "$off"
else
  printf '%sFull checks%s\n' "$bold" "$off"
  [ -z "$(git status --porcelain)" ] || echo "Note: this checks the working tree, uncommitted changes included."
fi

section "Repository"
check "LF line endings" line_endings

if touches '\.go$|^go\.(mod|sum)$|^staticcheck\.conf$'; then
  section "Go"
  if need go "Install it from https://go.dev/dl/." >/dev/null; then
    check "gofmt" gofmt_check
    check "go vet" linux go vet ./...
    check "staticcheck" staticcheck_check
    if ! ((quick)); then
      check "govulncheck: no known vulnerabilities" govulncheck_check
      check "tests" go_tests
      check "Go modules are on their latest releases" go_modules_latest
      check "Go is the latest release" go_toolchain_latest
    fi
  else
    fail "Go isn't installed"
  fi
fi

if touches '^web/'; then
  section "Web UI"
  if need npm "Install Node.js LTS from https://nodejs.org/." >/dev/null; then
    check "npm packages match the lockfile" web_deps
    check "svelte-check" web npm run check
    if ! ((quick)); then
      check "it builds" web_build
      check "npm audit: no known vulnerabilities" web npm audit --audit-level=low
      check "npm packages are on their latest releases" web npm outdated
      check "Node is the latest LTS" node_latest
      check "npm is the latest release" npm_latest
    fi
  else
    fail "Node.js isn't installed"
  fi
fi

if touches '^(deploy|scripts|\.githooks)/|\.sh$|^\.shellcheckrc$'; then
  section "Shell scripts"
  check "shellcheck" shellcheck_check
fi

if touches '^\.github/'; then
  section "GitHub workflows"
  if need go "Install it from https://go.dev/dl/." >/dev/null; then
    check "actionlint" actionlint_check
  else
    fail "Go isn't installed"
  fi
fi

if ! ((quick)); then
  section "Licences"
  if need go "Install it from https://go.dev/dl/." >/dev/null; then
    # The licences of what's built in, shipped with it (web/node_modules: the
    # Web UI checks install it).
    check "third-party notices are up to date" go run ./scripts/notices -check
  else
    fail "Go isn't installed"
  fi
fi

if ! ((quick)); then
  section "Docker image"
  check "Alpine is the latest release" alpine_latest
fi

echo
if ((${#failed[@]})); then
  printf '%s%d check(s) failed.%s From a git hook, --no-verify skips the checks once; only for emergencies.\n' "$red" "${#failed[@]}" "$off"
  exit 1
fi
printf '%sAll checks passed.%s\n' "$green" "$off"
