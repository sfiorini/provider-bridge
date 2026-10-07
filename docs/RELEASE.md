# Release

Provider Bridge versions live in three places, all derived from one source:

- **`VERSION`** (repo root) — the source of truth, format `x.y.z`.
- **The binary** — stamped at build time via ldflags into
  `providerbridge/internal/service/api` and surfaced by `providerbridge -version`,
  `GET /version`, and `GET /status`.
- **`CHANGELOG.md`** — Keep-a-Changelog; `[Unreleased]` folds into a versioned
  section on each release.

## Cutting a release

```bash
./scripts/release.sh --bump minor        # or patch|major, or an explicit x.y.z
```

The script: validates `VERSION`, computes the new semver, prints a preview
(`--dry-run` exits before changing anything), pre-flights (clean tree, tag
does not exist, in sync with `origin` — `--no-sync-check` skips that; tests
only if `go` is installed — otherwise tag CI is the gate), rewrites `VERSION`,
moves the `[Unreleased]` CHANGELOG entries into `## [x.y.z] - <date>`, commits
`release: vX.Y.Z`, tags `vX.Y.Z`, and pushes the branch and the tag. Any
failure after mutation rolls the commit/tag back.

## Tag → CI → GHCR → GitHub Release

Pushing `vX.Y.Z` triggers `.github/workflows/ci.yml`:

1. `test` and cross-platform `build` jobs run (binaries stamped from `VERSION`).
2. The `release` job verifies tag == `VERSION`, builds the Docker image with
   `--build-arg VERSION`, pushes
   `ghcr.io/sfiorini/provider-bridge:<version>` and `:latest`, extracts the
   `## [x.y.z]` CHANGELOG section as notes, publishes the GitHub Release
   (not a draft) and uploads the six platform archives.

For the first release from the seeded `0.1.0`, run
`./scripts/release.sh --bump 0.1.0` so the tag matches the already-seeded
`VERSION`.
