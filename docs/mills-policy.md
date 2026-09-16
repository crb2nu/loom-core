# Mills policy

Mills loads and validates its policy before starting work. Invalid initial
configuration prevents startup; an invalid hot reload is rejected and the last
known-good policy remains active.

## Pipeline concurrency

`max_concurrent_pipelines` limits simultaneous pipeline work. The field is
optional: when omitted, Mills uses the compiled default of
`DefaultPipelineConcurrencyLimit` (currently 8).

Explicit values must be integers in the inclusive range 1 through 256
(`policy.MinConcurrency` through `policy.MaxConcurrency`). Zero, negative
values, and values above the maximum are configuration errors. Mills does not
clamp an invalid policy value or interpret it as unlimited; the council runner
refuses admission before initializing its limiter or beginning work.

```yaml
max_concurrent_pipelines: 8
```

Legacy spellings remain readable for compatibility, but new policy documents
should use `max_concurrent_pipelines`.
