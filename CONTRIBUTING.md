# Contributing Guide

**English** | [中文](CONTRIBUTING.zh-CN.md)

Thanks for your interest in contributing to Absorb Reading Assistant. This file explains how to file issues, open PRs, set up the dev environment, and our code conventions.

## Code of Conduct

Be kind and stick to the topic. Constructive discussion and critique are welcome.

## How to start

1. File an issue: for bugs, feature ideas, or design discussion, search existing issues first.
2. Claim / discuss: comment on the issue about your plan to avoid duplicate work.

## Development setup

Requirements: Go 1.26+, Node.js 24+, [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation), Windows 10/11 (WebView2 Runtime).

```bash
make build      # Build the full desktop artifacts (build/bin)
make build-cli  # Build the CLI console version
make run        # Build and launch the desktop app
make test       # Go unit tests + frontend/extension vitest suite
```

Use `make run-cli` to start the headless CLI service when debugging the extension link.

## Code conventions

- Go: follow `gofmt` / `go vet`; place new logic in the appropriate `internal/` submodule.
- Frontend / extension: Vue 3 + Vite + Tailwind; components and scripts should have vitest coverage.
- Commit messages: describe *what* and *why* clearly, in Chinese or English.

## Design decisions

Product definition, roadmap, and the absorption-loop plan live in `design/` (`orientation.md`, `ld.md`). For changes touching user flows or the data model, align with the design docs before coding.

## PR workflow

1. Fork and create a feature branch.
2. Make sure `make test` passes.
3. Open a PR describing the motivation, changes, and how to test; link the related issue.
