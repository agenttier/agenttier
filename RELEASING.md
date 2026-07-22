# Releasing AgentTier

The canonical release workflow. Follow this every time you cut a new version.

## Before you tag

Ask the user which bump to apply: **patch** / **minor** / **major**.
Never decide unilaterally.

| Bump | Example | When |
| --- | --- | --- |
| patch | v0.1.0 → v0.1.1 | Bug fixes, doc-only, internal refactors, dep bumps |
| minor | v0.1.1 → v0.2.0 | New features, new CRD fields, new endpoints, non-breaking behavior changes |
| major | v0.x.y → v1.0.0 | Breaking API / CRD / endpoint changes |

Release-candidate tags are `vX.Y.Z-rcN`. Final GA tags have no suffix.

## Pre-release checklist

Work through this top-to-bottom on a clean `main`. Everything must pass
before you push the tag.

### Code and tests

- [ ] `main` is green on CI (lint, test 1.29 / 1.30 / 1.31, build, python-sdk, docs, security)
- [ ] `make verify-codegen` — generated deepcopy + CRD YAML is current
- [ ] `make test` — unit tests pass locally (skip on macOS if the LC_UUID issue bites; CI covers it)
- [ ] `make lint` — golangci-lint clean
- [ ] `./scripts/check-license-headers.sh` — every Go file has the Apache header
- [ ] `cd web-ui && npm ci && npm run lint && npm run build` — web UI builds and types check
- [ ] `cd python-sdk && pip install -e ".[dev]" && pytest && mypy --strict src/agenttier/` — SDK smoke tests and strict type check pass
- [ ] `cd python-sdk && pip install -e ".[mcp]"` — the optional MCP extra installs cleanly and registers `agenttier-mcp`/`agenttier-mcp-http` console scripts (`which agenttier-mcp`)

### Helm chart

- [ ] `helm lint helm/agenttier/`
- [ ] `helm template agenttier helm/agenttier/` renders without error

### Docs

- [ ] `cd docs && mkdocs build --strict` — no broken nav or missing pages
- [ ] New or changed features have a corresponding `docs/docs/` page (deeper coverage than the README one-liner). The README features audit below covers the README-side check separately.

### Versioning (all four must match)

- [ ] `helm/agenttier/Chart.yaml` — `version` and `appVersion` updated
- [ ] `VERSION` — bare version updated (e.g. `0.9.0`, no `v` prefix; `deploy.sh`/`scripts/lib/version.sh` derive image tags from it, so a stale value deploys old-looking tags from source builds)
- [ ] `python-sdk/pyproject.toml` — `version` updated
- [ ] `python-sdk/src/agenttier/_version.py` — `__version__` constant updated (this is imported by the SDK's runtime and `User-Agent` header; hatchling reads pyproject.toml separately)
- [ ] (Go: version is set via ldflags in `make build`; no source change)

### Changelog and README

- [ ] `CHANGELOG.md` — `[Unreleased]` rolled into a new `[vX.Y.Z] - YYYY-MM-DD` section; empty `[Unreleased]` stays on top
- [ ] **`README.md` features audit (release blocker for major features)** — walk every commit since the last release tag with `git log <last-tag>..HEAD --format="%h %s"`. For each commit, classify as:
  - **Major feature (MUST be in README's `## Features` section):** new user-visible capability, new endpoint surface, new reference image, new top-level Helm flag, new SDK / CLI command, new architectural primitive (cloning, observability pipeline, retention policy, etc), or any item that just flipped from Open to Done on the GitHub Project board.
  - **Minor change (skips README):** bug fixes, internal refactors, dep bumps, test-only changes, lint / CI tweaks, doc-only edits.
  
  For every major-feature commit, grep `README.md` for a phrase that names the capability. Add a bullet to the right Features subsection if missing — match the existing prose style. Then trim anything from the **`### On the roadmap`** block that just shipped this cycle (stale roadmap entries make the project look stuck).
- [ ] `README.md` install commands still valid (helm repo URL, image tags, pip install)
- [ ] `todo.md` — relevant tasks marked `[x]` or `[~]` with a note on what shipped

### Secrets and repo settings

- [ ] `PYPI_TOKEN` is set in the org-scoped Actions secrets (required for SDK publish; without it the step skips gracefully)
- [ ] Org Packages setting allows Public container visibility (one-time; done)
- [ ] GitHub Pages is enabled and pointed at `gh-pages` (one-time; done)

## Releasing

Tag:

```bash
git tag vX.Y.Z
git push --tags
```

Watch the `Release` workflow run. It produces:

- Multi-arch images at `ghcr.io/agenttier/{controller,router,web-ui,sandbox-general,sandbox-claude-code,sandbox-minimal}:vX.Y.Z` (and `:latest` on GA only)
- Cosign signatures + SPDX & CycloneDX SBOMs attached to every image
- Helm chart tarball + updated `index.yaml` at `https://agenttier.github.io/agenttier/charts/`
- MkDocs site at `https://agenttier.github.io/agenttier/`
- CLI binaries for linux/darwin/windows × amd64/arm64 attached to the GitHub Release
- Python wheel + sdist published to PyPI (if `PYPI_TOKEN` is set) or attached as workflow artifacts
- GitHub Release with a templated body (install snippets + auto-generated notes)

## Post-release verification

Anonymous checks a user would run:

```bash
docker pull ghcr.io/agenttier/controller:vX.Y.Z   # replace X.Y.Z with the new version
helm repo update && helm search repo agenttier --versions
curl -sL https://agenttier.github.io/agenttier/ | grep -q AgentTier
pip install agenttier==X.Y.Z   # if PyPI publish is on; replace X.Y.Z
```

If any of these 404 or fail, investigate the workflow run before announcing.

## Post-release cleanup sweep

The `release-retention` workflow job (and `scripts/release-retention.sh <tag>`) runs after
`github-release` and keeps the repo tidy. It keeps the **latest 10** releases and **tags** and
prunes the rest:

- **GitHub Releases** older than the latest 10 are **deleted** (Release page + CLI binaries).
- **Git tags** beyond the latest 10 versions are **deleted** (`go install`/`helm`/`docker`
  by-tag stop resolving for those old versions; the underlying commits stay).
- **ghcr.io images** for pruned release tags + untagged manifests >30 days are deleted.
- **Helm chart index** (`gh-pages` `/charts/index.yaml`) is trimmed to the latest 10.
- **github-pages deployments** are pruned to the latest 10.
- **`dependabot/*` branches** with no open PR are deleted.
- **Never pruned:** PyPI versions, cosign signatures/SBOMs of kept images, and the underlying
  git commits — forever.

**Open PRs are triaged on every release** — *always* list the open PRs *before* tagging and
address each (take safe Dependabot bumps in-tree and supersede, merge ready human PRs, defer
held/blocked majors with a written reason), then close the resolved ones in this sweep. The
agent-side `release-workflow` skill has the full mechanics, env knobs (`KEEP_COUNT`,
`KEEP_TAG_COUNT`, `KEEP_DEPLOYMENTS`), and `--dry-run`.

## First-release-only chores

These only apply when the release produces artifacts for the first time:

- **New ghcr.io packages default to Private.** Flip each via
  `https://github.com/orgs/agenttier/packages/container/package/<name>/settings`
  → Change visibility → Public.
- **PyPI project claim.** The very first PyPI upload claims the name. Subsequent
  uploads use the scoped token you configure in the repo secrets.

## Rollback

If a release is unusable:

1. Don't delete the tag (it stays pointing at a real commit).
2. Tag a new patch release that reverts the offending commits.
3. Mark the bad release as a pre-release in the GitHub UI so it stops being the "latest."
4. If images are actively harmful, flip their visibility back to Private until a fix ships.
