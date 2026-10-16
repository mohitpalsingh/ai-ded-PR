#!/usr/bin/env bash
set -euo pipefail

# Remove ai-ded-pr binaries, cache, state, logs and symlink.

echo "==> Removing installed binary"
for d in "${HOME}/.local/bin" "/usr/local/bin"; do
  target="${d}/ai-ded-pr"
  if [ -L "${target}" ]; then
    if [ -w "${d}" ]; then
      rm -f "${target}"
    else
      sudo rm -f "${target}"
    fi
  fi
done

echo "==> Removing cache, state and models"
rm -rf "${HOME}/.cache/ai-pr-review"

echo "==> Removing logs"
rm -f /tmp/ai-pr-review*.log

echo "==> Done. Homebrew packages (go, llama.cpp) are left intact."