#!/usr/bin/env bash
# Installs a native git pre-commit hook (no extra tooling needed). For the richer
# set (incl. gitleaks), use the pre-commit framework instead:
#   pip install pre-commit && pre-commit install
set -euo pipefail
cd "$(dirname "$0")/.."

hook=.git/hooks/pre-commit
mkdir -p .git/hooks
cat >"$hook" <<'HOOK'
#!/usr/bin/env bash
set -euo pipefail
echo "pre-commit: gofmt / vet / test + secret scan"

u="$(gofmt -l cmd internal)"
if [ -n "$u" ]; then echo "✗ needs gofmt:"; echo "$u"; exit 1; fi

go vet ./...
go test -short ./...

# Cheap secret scan on staged changes: catch obvious key/token shapes before they
# land. Not a substitute for gitleaks, but zero-dependency.
staged="$(git diff --cached --name-only --diff-filter=ACM | grep -vE '\.(md|lock)$|go\.sum' || true)"
if [ -n "$staged" ]; then
  if git diff --cached -- $staged | grep -nEi '(sk-[a-z0-9]{16,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|api[_-]?key["'\'' ]*[:=]["'\'' ]*[a-z0-9]{16,})' ; then
    echo "✗ possible secret in staged changes — remove it or use \$ENV/secrets.json"
    exit 1
  fi
fi
echo "✓ pre-commit passed"
HOOK
chmod +x "$hook"
echo "installed $hook"
