#!/usr/bin/env bash
set -euo pipefail

# Remove ai-ded-pr binaries, cache, state, logs and symlink.

echo "==> Removing installed binary"
if [ -L /usr/local/bin/ai-ded-pr ]; then
  if [ -w /usr/local/bin ]; then
    rm -f /usr/local/bin/ai-ded-pr
  else
    sudo rm -f /usr/local/bin/ai-ded-pr
  fi
fi

echo "==> Removing cache, state and models"
rm -rf "${HOME}/.cache/ai-pr-review"

echo "==> Removing logs"
rm -f /tmp/ai-pr-review*.log

echo "==> Done. Homebrew packages (go, llama.cpp) are left intact."