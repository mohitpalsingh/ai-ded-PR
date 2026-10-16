#!/usr/bin/env bash
set -euo pipefail

# Install ai-ded-pr on macOS (Apple Silicon / Metal optimised).
# - Installs prerequisites via Homebrew (go, llama.cpp)
# - Builds the binary and links it into ~/.local/bin (no sudo)
# - Downloads the default local models into ~/.cache/ai-pr-review/models

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODEL_DIR="${HOME}/.cache/ai-pr-review/models"
LLM_MODEL="${MODEL_DIR}/gemma-3-4b-it-Q4_K_M.gguf"
LLM_URL="https://huggingface.co/ggml-org/gemma-3-4b-it-GGUF/resolve/main/gemma-3-4b-it-Q4_K_M.gguf"
EMBED_MODEL="${MODEL_DIR}/embeddinggemma-300m-qat-Q8_0.gguf"
EMBED_URL="https://huggingface.co/ggml-org/embeddinggemma-300m-qat-q8_0-GGUF/resolve/main/embeddinggemma-300m-qat-Q8_0.gguf"

echo "==> Installing prerequisites"
if ! command -v brew >/dev/null 2>&1; then
  echo "Homebrew is required. Install from https://brew.sh and re-run." >&2
  exit 1
fi
brew install go llama.cpp 2>&1 | tail -n 5

echo "==> Building ai-ded-pr"
cd "${ROOT_DIR}"
make build

echo "==> Creating model cache"
mkdir -p "${MODEL_DIR}"

download() {
  local url="$1" dest="$2" name="$3"
  if [ -f "$dest" ]; then
    echo "   $name already present; skipping"
    return
  fi
  echo "   Downloading $name (resumes if interrupted)..."
  curl -L --fail --progress-bar -C - -o "$dest.part" "$url" && mv "$dest.part" "$dest"
}

download "$LLM_URL" "$LLM_MODEL" "Gemma 3 4B Q4"
download "$EMBED_URL" "$EMBED_MODEL" "EmbeddingGemma 300M Q8 (optional semantic aid)"

echo "==> Linking into ~/.local/bin"
BIN_DIR="${HOME}/.local/bin"
mkdir -p "${BIN_DIR}"
ln -sf "${ROOT_DIR}/build/ai-ded-pr" "${BIN_DIR}/ai-ded-pr"
case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *) echo "   NOTE: add ${BIN_DIR} to your PATH:"; echo "         export PATH=\"${BIN_DIR}:\$PATH\"";;
esac

echo "==> Done. Run: ai-ded-pr --help"
