# 🧠 ai-ded-pr

**Stop reading PRs file-by-file. A local Gemma model plans the review, and you walk it one file at a time.**

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.27+](https://img.shields.io/badge/go-1.27%2B-00ADD8.svg)](https://go.dev)
[![Platform: macOS Apple Silicon](https://img.shields.io/badge/platform-macOS%20Apple%20Silicon-black.svg)](#-requirements)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](#-extend)
![No telemetry](https://img.shields.io/badge/telemetry-none-success.svg)

On-device. No accounts. Nothing ever leaves your machine.

```text
  ai-ded-pr v1.0.5  guided code review with local Gemma
  ┌─────────────────────────────┬──────────────────────────────────────────────┐
  │ ✓ 1 User.java               │ Rationale: - Intent: Adds OAuth refresh ...  │
  │ → 2 OrderService.java       │ - Dependencies: TokenService, Redis cache    │
  │ • 3 OrderTest.java          │ - Verify: expiry math, nil token handling    │
  │ • 4 application.properties  │ - Risk: Token refresh loops on clock skew    │
  │                             │                                              │
  │ progress: 1/4               │ ------------- diff ------------              │
  │ n/p move · m done · / ask   │ + if (token.expired()) refresh();            │
  └─────────────────────────────┴──────────────────────────────────────────────┘
```

## ⚡ Why

- **A plan, not a dump.** Files are ordered to minimize cognitive load — types → core logic → callers → tests → config — each with a 4-bullet rationale: *Intent / Dependencies / Verify / Risk*.
- **One model call per PR.** The whole plan (order + risks + per-file summaries) comes from a single local LLM pass, then it's *persisted*. Swapping files never regenerates anything.
- **Local and boring.** Gemma 3 4B on llama.cpp (Metal). ~2.5 GB model, starts when the TUI opens, killed on exit. No daemon, no ports left behind.

## 🔧 Requirements

- macOS on Apple Silicon (M-series), 16 GB+ RAM
- [Homebrew](https://brew.sh) Go + llama.cpp: `brew install go llama.cpp`

## 📦 Install (one command)

```bash
git clone https://github.com/mohit/ai-ded-pr.git
cd ai-ded-pr
./scripts/install.sh        # builds, links into ~/.local/bin, downloads the model
```

The model (~2.5 GB) is fetched once with resume support; if a download is
interrupted, the next run continues where it stopped.

## 🚀 Use

```bash
ai-ded-pr                         # review current repo changes
ai-ded-pr diff                    # same, explicit
ai-ded-pr branch-diff feat main   # branch vs base
ai-ded-pr ask "where is auth handled?"   # find relevant files
```

GitHub PRs:
```bash
ai-ded-pr --github https://github.com/owner/repo/pull/123
```

### Keys
`n`/`p` next/prev file · `j`/`k` scroll · `m` done · `s` skip · `/` ask · `r` replan · `?` help · `q` quit

Every file gets the same four-bullet summary — **Intent / Dependencies / Verify / Risk** — generated once by your model, persisted, never recalculated.

## 🧩 How it works

```text
  git diff / GitHub PR ──► changed files + hunks
          │
          ▼
  local llama.cpp (Metal) ──► review plan: order + 4-bullet rationale per file
          │
          ▼
  TUI ──► walk files one by one · mark done/skip · ask questions
          │                       (state persisted per repo+commit)
          ▼
  q / Ctrl+C ──► server killed, ports released, zero residue
```

## 💾 Footprint

| | |
|---|---|
| Model | Gemma 3 4B Q4_K_M (~2.5 GB, one-time download) |
| Runtime RAM | ~3 GB while the TUI is open (quantized KV cache, 8k context) |
| Processes | `llama-server` starts with the TUI, dies with it — verified by `make regression` |
| Cache/state | `~/.cache/ai-pr-review/` (state per repo+commit, resumable model download) |

## 🧹 Uninstall (zero bloat)

```bash
./scripts/uninstall.sh
```

Removes the binary, models, cache, state, and logs. Leaves your Homebrew `go`/`llama.cpp` intact.

## 🛠 Extend

Hosts are pluggable: implement `hosts.Host` in `internal/hosts/<name>` for GitLab, Bitbucket, or Azure DevOps. PRs welcome.

## 🧪 Develop

```bash
make build        # binary in build/
make test         # go test + vet
make regression   # full end-to-end suite against a real repo + real model
```

## Status

v1.0.5 — MIT License
