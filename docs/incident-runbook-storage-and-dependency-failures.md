# Storage and dependency failure runbook

## LiteLLM gateway reports missing authentication

### Symptom

Calls through the shared FlexInfer client fail immediately with:

```
flexinfer: LiteLLM API key is not configured; configure the gateway API key
```

This is a local preflight guardrail. No request was sent to LiteLLM and the
circuit breaker was not updated, so treat it as a configuration issue rather
than a provider outage.

### Recovery

1. Confirm that the configured endpoint is the LiteLLM gateway rather than the
   keyless FlexInfer proxy.
2. Provision the gateway credential in the process or workload environment:
   use `LOOM_MILLS_LITELLM_KEY` for Mills LiteLLM workloads or
   `FLEXINFER_API_KEY` for FlexInfer/weaver workloads. Restart the affected
   service after updating its configuration.
3. If the workload is intended to use the keyless proxy, set
   `FLEXINFER_PROXY_URL`; the client will use that route when no gateway key is
   configured.
4. Retry the operation and investigate remote LiteLLM errors only after this
   local configuration error is resolved.
