# Release Process

This repository publishes two independently versioned controllers from the
same source tree:

- **TSC controller** (`controllers/tsc-controller`)
- **JVM probe controller** (`controllers/jvm-probe-controller`)

Each controller has its own semantic version, its own Helm chart, and its
own container image. Releases are cut from `main` by pushing a Git tag of
the form `<chart-prefix>-v<semver>`; GitHub Actions then builds the
matching image and publishes it.

## Tag naming convention

| Controller   | Tag prefix       | Example tag    |
| ------------ | ---------------- | -------------- |
| TSC          | `tsc-v`          | `tsc-v0.2.16`  |
| JVM probe    | `jvm-v`          | `jvm-v0.0.18`  |

The semver portion of the tag is the version that will be:

1. Baked into the controller image (as `OPERATOR_VERSION`).
2. Used as the default image tag in the Helm chart's `image.tag`.
3. Published as a GitHub release (one per tag, named after the tag).

The two prefixes are intentionally disjoint so the two controllers can be
released independently and a JVM-probe rollout does not imply a TSC
rollout (and vice versa). Multiple tags can be pushed in a single commit
when both controllers are ready.

## Tagging a release

```bash
git checkout main
git pull origin main

# Bump the TSC chart version and create the tag.
git tag tsc-v0.2.16

# Bump the JVM probe chart version and create the tag.
git tag jvm-v0.0.18

# Push both tags in a single push. The release workflows trigger on the
# tag push and run concurrently.
git push origin tsc-v0.2.16 jvm-v0.0.18
```

There is no separate "release branch" — tags are pushed directly from
`main`. If a release needs to be retracted, delete the tag from GitHub
and force-push the rewritten history only if absolutely necessary; in
general prefer cutting a follow-up patch release.

## What the workflows do

- **Release workflows** (`.github/workflows/release-*.yaml`): triggered by
  the matching `*-v*` tag push. They build the controller image for the
  tagged commit and push it to both GCP Artifact Registry and GitHub
  Container Registry:

  ```
  us-docker.pkg.dev/castai-hub/library/castai-tsc-controller:<tag-without-prefix>
  us-docker.pkg.dev/castai-hub/library/castai-jvm-probe-controller:<tag-without-prefix>
  ghcr.io/castai/images/castai-tsc-controller:<tag-without-prefix>
  ghcr.io/castai/images/castai-jvm-probe-controller:<tag-without-prefix>
  ```

  For example, the tag `tsc-v0.2.16` produces
  `us-docker.pkg.dev/castai-hub/library/castai-tsc-controller:0.2.16` and
  `ghcr.io/castai/images/castai-tsc-controller:0.2.16`. The same workflow
  also publishes a matching GitHub release with the rendered notes.

- **`ci-main.yaml`** runs on every push to `main` (and on pull requests
  targeting `main`) and pushes images tagged `latest` to the same
  registries. The `latest` tag is **not**
  semantically versioned — it always points at the most recent successful
  `main` build and should not be relied on for reproducible deployments.
  For production installs, pin to an explicit `tsc-vX.Y.Z` /
  `jvm-vX.Y.Z` image tag.

## Pre-release checklist

1. Bump the `appVersion` and `version` fields in the affected chart's
   `Chart.yaml`.
2. Update the chart's `values.yaml` `image.tag` if you want the new tag
   to be the default for fresh installs (otherwise leave it for the
   next release — the rendered manifest always reads from `image.tag`
   or the chart `appVersion` fallback).
3. Run `helm lint` and `helm template` against the chart you are about
   to release.
4. For Go changes, run `go test -race -count=1 ./...` in the affected
   controller directory.
5. Confirm the `ci-main.yaml` run for the release commit is green.
6. Tag and push as shown above.
