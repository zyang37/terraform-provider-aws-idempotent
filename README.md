# terraform-provider-aws-idempotent

A build of the Terraform AWS provider that sends deterministic, journaled
idempotency tokens, so a killed `terraform apply` can be re-run without
creating duplicate resources.

- [Development plan and test plan](docs/DEV_PLAN.md)
- [Supported AWS APIs](docs/SUPPORTED_APIS.md) (generated)
- `tools/gen-idempotent-ops`: builds the operation table from AWS SDK Smithy models
- `tools/codemod-tokens`: rewrites upstream random tokens to the idempotency sentinel
