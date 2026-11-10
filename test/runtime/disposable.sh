#!/bin/bash
# Run after zoo up on a disposable Docker engine. Never use a production keeper.
# ZOO_BIN must be absolute. ZOO_HOME and ZOO_KEYS_FILE must select test state.
# Dollar expressions in guest commands must expand inside the cage.
# shellcheck disable=SC2016
set -euo pipefail
: "${ZOO_BIN:?set the absolute path to the test zoo binary}"
: "${ZOO_HOME:?set a separate test state directory}"
if [ "${ZOO_KEYS_FILE:-}" != 1 ]; then
  echo 'This test requires ZOO_KEYS_FILE=1.' >&2
  exit 1
fi
case "$ZOO_BIN" in /*) ;; *) echo 'ZOO_BIN must be absolute.' >&2; exit 1 ;; esac
fixture=$(mktemp -d "${ZOO_TEST_ROOT:-/tmp}/zoo-disposable.XXXXXX")
cd "$fixture"
git init -q
printf 'original\n' > README.md
printf 'SECRET=test-only\n' > .env
printf '.env\n' > .gitignore
git add README.md .gitignore
git -c user.name=Test -c user.email=test@localhost commit -qm baseline

"$ZOO_BIN" task new runtime-smoke .
"$ZOO_BIN" task run runtime-smoke shell -- -c '
  set -eu
  test ! -e .env
  printf "agent edit\n" > README.md
  printf "new file\n" > added.txt
  git add README.md added.txt
  git -c user.name=Test -c user.email=test@localhost commit -qm edit
  printf "uncommitted\n" >> added.txt
'
cage=$("$ZOO_BIN" task list | awk '$1 == "runtime-smoke" {print $2}')
test "$(docker inspect --format '{{.State.Running}}' "zoo-$cage")" = false
"$ZOO_BIN" task diff runtime-smoke | grep -q '+uncommitted'
"$ZOO_BIN" task apply runtime-smoke --dry-run
test "$(cat README.md)" = original
"$ZOO_BIN" task apply runtime-smoke
test "$(cat README.md)" = 'agent edit'
grep -q uncommitted added.txt
grep -q SECRET=test-only .env
"$ZOO_BIN" task destroy runtime-smoke
test ! -d "$ZOO_HOME/tasks/runtime-smoke"
test ! -d "$ZOO_HOME/cache/project/$cage"

printf 'network:\n  allow: [api.github.com]\n' > zoo.yaml
"$ZOO_BIN" trust
"$ZOO_BIN" task new runtime-policy .
"$ZOO_BIN" task run runtime-policy shell -- -c '
  set -eu
  test "$(curl -sS -m 20 -o /dev/null -w "%{http_code}" https://api.github.com)" = 200
  if out=$(curl -sS -m 15 -o /dev/null https://registry.npmjs.org 2>&1); then exit 1; fi
  printf "%s" "$out" | grep -q 403
  printf "network:\n  allow: [registry.npmjs.org]\n" > zoo.yaml
'
"$ZOO_BIN" task run runtime-policy shell -- -c '
  set -eu
  if out=$(curl -sS -m 15 -o /dev/null https://registry.npmjs.org 2>&1); then exit 1; fi
  printf "%s" "$out" | grep -q 403
'
"$ZOO_BIN" task destroy runtime-policy --discard

printf 'network:\n  allow: []\n' > zoo.yaml
"$ZOO_BIN" trust
"$ZOO_BIN" task new runtime-deny .
"$ZOO_BIN" task run runtime-deny shell -- -c '
  set -eu
  if out=$(curl -sS -m 15 -o /dev/null https://api.github.com 2>&1); then exit 1; fi
  printf "%s" "$out" | grep -q 403
'
"$ZOO_BIN" task destroy runtime-deny
# Synthetic key only: exercise the real credential bridge without a paid model call.
printf 'test-only-invalid-model-key\n' | "$ZOO_BIN" keys set codex
printf 'network:\n  allow: [api.openai.com]\n' > zoo.yaml
"$ZOO_BIN" trust
"$ZOO_BIN" task new runtime-credential .
"$ZOO_BIN" task run runtime-credential codex -- --version
"$ZOO_BIN" task run runtime-credential shell -- -c '
  set -eu
  ph=$(node -p "require(process.env.HOME + \"/.codex/auth.json\").OPENAI_API_KEY")
  case "$ph" in sk-zoocred.codex.*) ;; *) echo "missing placeholder"; exit 1 ;; esac
  if grep -q test-only-invalid-model-key "$HOME/.codex/auth.json"; then exit 1; fi
  code=$(curl -sS -m 25 -o /tmp/credential-response.json -w "%{http_code}" -H "Authorization: Bearer $ph" https://api.openai.com/v1/models)
  if [ "$code" != 401 ]; then printf "Expected upstream 401, got %s\n" "$code" >&2; exit 1; fi
'
"$ZOO_BIN" task destroy runtime-credential
"$ZOO_BIN" keys rm codex
printf 'PASS: disposable lifecycle, masks, private cache cleanup, scoped policy, unapproved edits, deny-all, and brokered HTTPS\n'
# Preserve the fixture on failure for diagnosis; remove only this test's files.
rm -rf "$fixture"
