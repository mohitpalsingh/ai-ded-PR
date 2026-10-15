# 🧠 ai-ded-pr — Guided AI Code Review in Your Terminal

**Stop reading PRs file-by-file. Let a local Gemma model build the review path, then walk you through it one file at a time.**

On-device. Fast. No accounts. No uploads.

```text
  Guided PR Review
  ┌──────────────────────┬──────────────────────────────────────────┐
  │ ✓ 1 User.java        │ Rationale: ...                          │
  │ → 2 OrderService.java│ Intent: ...                             │
  │ • 3 OrderTest.java   │ Risk: ...  Verify: ...                  │
  │                      │                                          │
  │ progress: 1/3        │ ----------- diff -------------           │
  └──────────────────────┴──────────────────────────────────────────┘
```

## ⚡ Install (one command)

```bash
git clone https://github.com/<your-org>/ai-ded-pr.git
cd ai-ded-pr
./scripts/install.sh
```

Or build manually:
```bash
make build
make install
```

## 🚀 Use

```bash
ai-ded-pr                        # review current repo changes
ai-ded-pr diff                   # same, explicit
ai-ded-pr branch-diff feat main  # compare a branch to a base
```

GitHub PRs:
```bash
ai-ded-pr --github https://github.com/owner/repo/pull/123
```

### Keys
`n`/`p` next/prev file · `j`/`k` scroll · `m` done · `s` skip · `/` search · `r` replan · `?` help · `q` quit

## 🧹 Uninstall (zero bloat)

```bash
./scripts/uninstall.sh
```
Removes the binary, models, cache, state, and logs. Leaves your Homebrew `go`/`llama.cpp` intact.

## 🧩 How it works
- **GitHub PRs** or **local diffs/branches** → normalized changed files + hunks
- **Local Gemma 3 (4B, Q4)** on `llama.cpp` (Metal) proposes a review order + rationale + risks
- **Optional EmbeddingGemma 300M** adds semantic search/clustering to aid (never exposed directly)
- **Heuristic fallback** if no model is present

## 💾 Footprint
- LLM: ~2.4 GB (downloaded once)
- Embeddings (optional): ~0.4 GB
- Runtime: starts only while the TUI is open, killed on exit
- Isolated ports/cache; does not touch other local LLM tools

## 🛠 Extend
Implement `hosts.Host` in `internal/hosts/<name>` for GitLab/Bitbucket/Azure DevOps.

## Status
v1.0.0 — MIT License
```