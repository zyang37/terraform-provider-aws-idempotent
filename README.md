<!-- Copyright IBM Corp. 2014, 2026 -->
<!-- SPDX-License-Identifier: MPL-2.0 -->

<!-- markdownlint-disable first-line-h1 no-inline-html -->
<a href="https://terraform.io">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/terraform_logo_dark.svg">
    <source media="(prefers-color-scheme: light)" srcset=".github/terraform_logo_light.svg">
    <img src=".github/terraform_logo_light.svg" alt="Terraform logo" title="Terraform" align="right" height="50">
  </picture>
</a>

# terraform-provider-aws-idempotent

A fork of the [Terraform AWS Provider](https://registry.terraform.io/providers/hashicorp/aws/latest/docs)
that sends deterministic, journaled idempotency tokens on every AWS API call
that supports one, instead of a random token per apply. If `terraform apply`
is killed after a request reaches AWS but before Terraform records the
result in state, the next apply resends the same token, AWS returns the
resource it already created, and no duplicate is made.

Baseline: `hashicorp/terraform-provider-aws` v6.64.0, vendored unmodified so
the idempotency changes are reviewable as a diff. See
[docs/DEV_PLAN.md](docs/DEV_PLAN.md) for the design, verified facts, and
current implementation status.

- [Development plan and test plan](docs/DEV_PLAN.md)
- [Supported AWS APIs](docs/SUPPORTED_APIS.md) (generated)
- `internal/idempotency`: the deterministic token journal and SDK middleware
- `tools/gen-idempotent-ops`: builds the operation table from AWS SDK Smithy models
- `tools/codemod-tokens`: rewrites upstream random tokens to the idempotency sentinel

This is an independent fork, not affiliated with or endorsed by HashiCorp.
For the upstream provider's own documentation, contributing guide, and
security policy, see [hashicorp/terraform-provider-aws](https://github.com/hashicorp/terraform-provider-aws).
