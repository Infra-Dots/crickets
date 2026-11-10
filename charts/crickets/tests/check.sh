#!/usr/bin/env bash
# Lint + render both value sets and diff against goldens. UPDATE=1 rewrites them.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
for set in defaults override; do
  args=(); golden=tests/golden.yaml
  [ "$set" = override ] && { args=(-f ci/override-values.yaml); golden=tests/golden-override.yaml; }
  helm lint --strict ${args[@]+"${args[@]}"} .
  out=$(helm template crickets . --namespace crickets ${args[@]+"${args[@]}"})
  if [ "${UPDATE:-}" = 1 ]; then
    printf '%s\n' "$out" > "$golden"
  elif ! diff -u "$golden" <(printf '%s\n' "$out"); then
    echo "golden drift: $golden (rerun with UPDATE=1)" >&2
    fail=1
  fi
done
# As a dependency Helm passes `global`; the root schema must allow it.
helm template crickets . --set global.probe=1 >/dev/null
exit $fail
