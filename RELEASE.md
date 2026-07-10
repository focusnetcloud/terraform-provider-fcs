# FCS Terraform Provider Release Process

This document describes the public release process for maintainers of
`terraform-provider-fcs`.

## Published Provider

- Terraform Registry: `registry.terraform.io/focusnetcloud/fcs`
- GitHub repository: `https://github.com/focusnetcloud/terraform-provider-fcs`
- Release tags: SemVer tags with a leading `v`, for example `v0.10.0`
- License: MPL-2.0

## Registry Requirements

Terraform Registry provider publishing requires:

- a public GitHub repository named `terraform-provider-fcs`
- a valid SemVer release tag such as `v0.10.1`
- release archives for supported platforms
- `SHA256SUMS`
- `SHA256SUMS.sig`
- a public GPG signing key registered for the `focusnetcloud` namespace

Official references:

- https://developer.hashicorp.com/terraform/registry/providers/publishing
- https://developer.hashicorp.com/terraform/registry/providers/docs

## Versioning

The first public release is `v0.10.0`.

Use:

- patch releases for fixes without schema or behavior changes, for example
  `v0.10.1`
- minor releases for new resources, new data sources, or behavior changes before
  `v1.0.0`, for example `v0.11.0`
- `v1.0.0` only after the public resource names, IDs, import behavior, and core
  attributes are stable

Never replace an already published provider version. If a published release is
wrong, publish a new version.

## Maintainer Setup

The release workflow expects these GitHub Actions repository secrets:

- `GPG_PRIVATE_KEY`: ASCII-armored private key for the release signing key
- `PASSPHRASE`: passphrase for the release signing key

The public half of the same GPG key must be registered as a signing key in the
Terraform Registry namespace `focusnetcloud`.

Do not commit or print:

- GPG private keys
- GPG passphrases
- GitHub tokens
- Terraform Registry session data
- FCS tenant tokens

## Preflight Checks

Run these checks before tagging a release:

```sh
git status --short --branch
go test ./...
TF_ACC=1 go test ./internal/provider -v -timeout 10m
make build
make vet
make vuln
make fmtcheck
terraform fmt -check -recursive examples
make docs
git diff --exit-code
```

Run a local GoReleaser snapshot:

```sh
goreleaser release --snapshot --clean --skip=publish,sign
```

Expected result:

- all tests pass
- generated docs are committed
- the worktree is clean
- snapshot artifacts are created under `dist/`
- no secret export files exist in the repository

## Create a Release

Create and push an annotated tag:

```sh
git tag -a v0.10.1 -m "v0.10.1"
git push origin v0.10.1
```

The tag push starts the GitHub Actions release workflow. GoReleaser builds the
provider archives, signs `SHA256SUMS`, creates the GitHub Release, and publishes
the release assets.

Watch the workflow:

```sh
gh run list --repo focusnetcloud/terraform-provider-fcs --workflow release.yml --limit 5
gh run watch --repo focusnetcloud/terraform-provider-fcs --exit-status
```

## Verify Release Assets

Download the release assets:

```sh
mkdir -p /tmp/fcs-provider-release
gh release download v0.10.1 \
  --repo focusnetcloud/terraform-provider-fcs \
  --dir /tmp/fcs-provider-release
```

Verify checksums:

```sh
cd /tmp/fcs-provider-release
shasum -a 256 -c terraform-provider-fcs_0.10.1_SHA256SUMS
```

Verify the checksum signature with the registered release public key:

```sh
gpg --verify \
  terraform-provider-fcs_0.10.1_SHA256SUMS.sig \
  terraform-provider-fcs_0.10.1_SHA256SUMS
```

## Registry Verification

After the Terraform Registry ingests the GitHub Release, verify the version:

```sh
curl -fsSL https://registry.terraform.io/v1/providers/focusnetcloud/fcs/versions
```

Run a clean registry install smoke test:

```sh
mkdir -p /tmp/fcs-provider-registry-smoke
cd /tmp/fcs-provider-registry-smoke

cat > main.tf <<'EOF'
terraform {
  required_providers {
    fcs = {
      source  = "focusnetcloud/fcs"
      version = "0.10.1"
    }
  }
}

provider "fcs" {
  endpoint = "https://api.focusnet.de"
}
EOF

terraform init
terraform providers
```

This smoke test only downloads the provider. It does not need an FCS API token
unless a plan or apply uses provider resources.

## Troubleshooting

If the GitHub release workflow fails while signing:

- confirm that `GPG_PRIVATE_KEY` and `PASSPHRASE` are set
- confirm that the private key is ASCII-armored
- confirm that the passphrase matches the key
- confirm that the public key is registered in Terraform Registry

If the Terraform Registry rejects or misses a release:

- confirm that the repository is public
- confirm that the release is not a draft or prerelease
- confirm that the tag is a valid SemVer tag with a leading `v`
- confirm that `SHA256SUMS.sig` signs the `SHA256SUMS` file
- confirm that the manifest is valid JSON

If documentation does not update:

- confirm that `docs/index.md` exists
- confirm that resource documentation exists under `docs/resources/`
- confirm that data source documentation exists under `docs/data-sources/`
- confirm that `make docs` was run before the release
- publish a new provider version; Registry documentation is versioned
