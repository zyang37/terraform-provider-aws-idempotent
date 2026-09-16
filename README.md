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
- `internal/idempotency`: the deterministic token journal and SDK middleware
- `tools/gen-idempotent-ops`: builds the operation table from AWS SDK Smithy models
- `tools/codemod-tokens`: rewrites upstream random tokens to the idempotency sentinel

## Supported AWS APIs

Generated from the Smithy models in aws-sdk-go-v2 (commit `4e0240a`) against
terraform-provider-aws `v6.64.0`, 2026-09-15. Full per-service, per-operation
tables: [docs/SUPPORTED_APIS.md](docs/SUPPORTED_APIS.md) (regenerate with
`tools/gen-idempotent-ops`).

| Set | Operations | Description |
|---|---|---|
| Tier 1 | 1439 across 195 services | Carries the `smithy.api#idempotencyToken` trait; aws-sdk-go-v2 auto-fills these with a random UUID if left empty, so our middleware just has to set a deterministic value first. |
| Tier 2 | 170 | Token-named member without the trait (e.g. ECS `CreateService`, Route 53 `CreateHostedZone`); the SDK does not touch these, so the provider must set them explicitly. |
| Tier 1 called by upstream provider today | 519 | Operations where the idempotent provider changes real `apply` behavior. |
| Tier 2 called by upstream provider today | 57 | Same, for Tier 2 operations. |

This is an independent fork, not affiliated with or endorsed by HashiCorp.
For the upstream provider's own documentation, contributing guide, and
security policy, see [hashicorp/terraform-provider-aws](https://github.com/hashicorp/terraform-provider-aws).
