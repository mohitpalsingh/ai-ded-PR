# ai-ded-pr (name TBD)

A local, AI-assisted code review TUI. It loads a GitHub pull request or a local git diff, runs a
small on-device Gemma model to produce a **guided review plan** (which file to read next and why),
then walks you through the files and their diffs one by one. Nothing leaves your machine except the
GitHub API calls needed to fetch a PR.

Inspired by DigUp's on-device EmbeddingGemma approach, adapted for *review* rather than search.

## Features (v1)

- Hosts: GitHub PRs (public, or private with `GITHUB_TOKEN`) and local git diffs.
- TUI only: two-pane layout, file/order list on the left, diff + AI summary on the right.
- Guided order: foundational types -> core logic -> callers/UI -> tests -> config/docs.
- Per-file AI summary with risk notes and focus checklist.
- Natural-language search across the changed files (`/`).
- Progress persistence under `~/.cache/ai-pr-review/state/`.
- Pluggable host interface so GitLab/Bitbucket can be added later.
- Local model via `llama.cpp` (`llama-server`), defaulting to Gemma 3 1B Q4 (lightweight).

## Install / build

```sh
# prerequisites
brew install go llama.cpp

cd ai-ded-pr
go build -o review ./cmd/review
```

## Usage

GitHub PR:

```sh
./review --github https://github.com/owner/repo/pull/123
```

Local diff:

```sh
./review --repo /path/to/repo --base HEAD~1 --head HEAD
./review diff                       # current repo, working changes (default)
./review branch-diff feature/x main # compare a branch against a base branch
```

Keys:

| Key | Action |
|---|---|
| `n` / `p` | next / previous file |
| `j` / `k` | scroll diff |
| `m` | mark current file done |
| `s` | skip current file |
| `u` | unmark |
| `/` | search changed files by natural language |
| `r` | regenerate review plan |
| `g` | new source |
| `?` | help |
| `q` | quit |

### Model

The default model (Qwen2.5-Coder 3B Q4, ~2.1GB) is downloaded on first request (Settings: press `d` in the welcome
screen) into `~/.cache/ai-pr-review/models/`. Override with `--model /path/to/*.gguf`.

For heavier reasoning you can point `--model` at a larger Gemma GGUF. Without a model the tool still works in
heuristic mode.

### Private GitHub repos

```sh
export GITHUB_TOKEN=ghp_...
./review --github ...
```

## Extending hosts

Implement `hosts.Host` in `internal/hosts/<name>` and register in the TUI. See `github` and `localgit`.

## Layout

```
cmd/review/        entrypoint + flags
internal/core/     diff parsing, models, planning, state
internal/hosts/     GitHub + local git adapters
internal/modelsvc/  llama.cpp server + download + client
internal/tui/      Bubble Tea interface
internal/config/   paths + defaults
```
