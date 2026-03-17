# Multi-Region AWS ASG Discovery

## Summary

This change extends the AWS cloud provider so Cluster Autoscaler can discover and manage Auto Scaling Groups across multiple AWS regions instead of assuming a single region.

The implementation keeps the existing single-region behavior as the default. Multi-region behavior is enabled by passing the new repeated `--aws-region` flag.

Example:

```text
--aws-region=us-east-1 --aws-region=us-west-2
```

If no `--aws-region` flags are provided, the provider continues to use the existing default AWS region resolution logic.

## Why This Was Structured This Way

The existing AWS provider was strongly single-region in three ways:

1. It built one AWS SDK config and one AWS client wrapper.
2. It refreshed ASGs through one cache using that single client.
3. It keyed AWS node groups by ASG name only.

That was fine for one region, but it breaks down immediately when two regions can contain the same ASG name.

Because of that, this PR does more than add a loop over regions:

1. It introduces a small region-aware client router.
2. It teaches the manager/cache paths to route reads and writes to the correct regional client.
3. It makes ASG identity region-aware when more than one region is configured.

The important compatibility choice is that ASG IDs remain name-only in single-region mode. That avoids breaking existing explicit node group specs (`--nodes=min:max:name`) and avoids changing current behavior for users who are not opting into multi-region discovery.

When multiple regions are configured, node group identity becomes `region/name`. That removes collisions while keeping the single-region path stable.

## What Changed

### Configuration

- Added `AWSRegions []string` to autoscaler options.
- Added a repeated `--aws-region` flag.
- Added normalization for configured regions:
  - trims whitespace
  - drops empty entries
  - de-duplicates values while preserving order

### AWS Client Construction

- Added a region-aware AWS service router abstraction.
- `BuildAWS` now:
  - resolves the configured region list
  - falls back to the default region if none are provided
  - builds one AWS wrapper per region
  - injects that router into the AWS manager

### Manager and Cache Behavior

- Added a region-aware manager constructor path.
- ASG refresh now fans out across all configured regions.
- Scale operations (`SetDesiredCapacity`, instance termination, scaling activity lookups) route to the correct region.
- Mixed instances policy EC2 lookups route to the ASG’s region.
- Managed nodegroup cache is now region-aware.

### Identity

- `AwsRef` now includes `Region`.
- In single-region mode:
  - `NodeGroup.Id()` is still just the ASG name.
- In multi-region mode:
  - `NodeGroup.Id()` becomes `region/name`.

This means two regions can now safely contain the same ASG name without one overwriting the other in the cache.

## Operational Impact

### AWS API Behavior

The main operational change is that refreshes now perform AWS API calls per configured region instead of once globally.

That means:

- Discovery refreshes will take longer overall when multiple regions are configured.
- The number of `DescribeAutoScalingGroups` calls increases roughly in proportion to the number of regions.
- The request load to AWS APIs increases accordingly.

This is expected. The refresh model is still the same top-level Cluster Autoscaler refresh loop, but each refresh now fans out across regions.

In practice, users should expect:

- longer refresh latency
- higher AWS API volume
- more opportunity for one region to be slower than another

The implementation does not introduce a new background watcher model. It keeps the existing polling model and applies it across multiple regional clients.

### Node Group Identity

In multi-region mode, node group IDs and any logs or status keyed by node group ID will use `region/name`.

This matters for:

- logs
- any automation that matches node groups by ID
- dashboards keyed by node group ID
- per-nodegroup CA metrics

Single-region users do not see this change.

## Monitoring Changes

No new AWS metric name was introduced, but the existing AWS request metric changed shape.

### Existing Metric Updated

`cluster_autoscaler_aws_request_duration_seconds`

Previous labels:

- `endpoint`
- `status`

Current labels:

- `endpoint`
- `status`
- `region`

This makes it possible to distinguish slow or failing AWS calls by region, which is important once the provider is talking to multiple regions.

### Expected Monitoring Effects

- Total AWS request counts will increase in multi-region mode.
- Latency distributions for AWS calls will widen because they now include multiple regions.
- You can now break down request rate, error rate, and latency by `region`.

### Dashboard / Alert Updates

Any queries that depended on the exact old label set should be updated to tolerate or use the new `region` label.

Examples:

- overall call rate:
  - aggregate by `endpoint` and `status`
- regional call rate:
  - aggregate by `endpoint`, `status`, and `region`
- regional latency:
  - aggregate histogram buckets by `endpoint`, `region`, and `le`

Also note that generic per-nodegroup metrics may now expose AWS node group IDs as `region/name` in multi-region mode.

## Test Coverage Added

The changes were built out with unit tests around both the new helper seams and the production-style constructor path.

### Multi-Region Cache / Routing Tests

Added tests covering:

- ASG auto-discovery across multiple regions
- multi-region auto-discovery with the same ASG name in two regions
- region-aware routing for `SetAsgSize`
- region-aware routing for `DescribeScalingActivities`
- managed nodegroup cache separation for same-named nodegroups in different regions

These tests validate the core correctness of:

- discovery fan-out
- cache isolation
- client routing
- region-qualified node group identity in multi-region mode

### Constructor-Path Tests

Added tests covering:

- `createAWSManagerWithServiceRouter(...)` discovering ASGs across multiple regions during initial refresh
- `createAWSManagerWithServiceRouter(...)` routing `SetAsgSize` to the correct regional client

These tests verify that the manager behaves correctly when built through the real region-aware constructor path, not only through ad hoc test setup.

### Helper / Config Tests

Added tests covering:

- region normalization
- AWS service router construction
- AWS request metric path with the new `region` label

## Validation Performed

The AWS provider test package passes with these changes.

Validated with:

```text
nix develop path:/home/bwilson/repos/autoscaler-codex --no-write-lock-file -c go test ./cloudprovider/aws -count=1
```

The flags package was also validated after the config plumbing changes.

## Reviewer Notes

The main compatibility-sensitive parts of this change are:

1. The decision to keep single-region IDs as plain names.
2. The shift to `region/name` only when multiple regions are configured.
3. The addition of the `region` metric label.

Those choices were made to:

- avoid breaking existing single-region users
- avoid cache collisions in multi-region mode
- make multi-region operational behavior observable

The next likely follow-up after merge is runtime validation in a real canary deployment with two regions, primarily to confirm expected refresh latency, AWS API volume, and regional error visibility in metrics.
