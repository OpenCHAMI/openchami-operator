# Relationship to the integration sandbox

The OpenCHAMI operator and the
[OpenCHAMI integration sandbox][sandbox-repo] are deliberately separate
test harnesses, covering non-overlapping concerns with different tooling.

[sandbox-repo]: https://github.com/OpenCHAMI/integration-sandbox

The **canonical** source of truth for the boundary — what each suite
covers, where state is shared (it isn't), how image versions are
selected, and when a change should land in both — lives in the sandbox
repo:

> [`OpenCHAMI/integration-sandbox` → `docs/relationship-to-operator.md`][sandbox-doc]

[sandbox-doc]: https://github.com/OpenCHAMI/integration-sandbox/blob/main/docs/relationship-to-operator.md

Operator-side rule of thumb:

- If you're chasing a problem in the **services**, run the sandbox.
- If you're chasing a problem in the **deployment** (CRDs, webhooks,
  NetworkPolicies, Gateway API, condition aggregation, RBAC), run the
  operator's e2e suite (`make test-e2e`). The sandbox does not exercise
  any of those.

For PR validation that needs to cross both — e.g. a service-side change
that the operator must also surface correctly — run them in parallel:

```bash
# In the sandbox repo:
make ci SBX_<NAME>_IMAGE=ghcr.io/openchami/<svc>:pr-N

# In this (operator) repo:
make test-e2e   # after pinning the relevant default image constant
```

See the operator's [`SERVICES.md`](../SERVICES.md) for the image-default
constants and [`docs/testing.md`](testing.md) for the full operator test
matrix.
