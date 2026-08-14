- **`test:reliability` auto-retries runner-contention SIGTERMs** (`.gitlab-ci.yml`):
  under ~8 concurrent MR pipelines the shared runner terminated the verifier
  build (make Error 143) and the job's explicit `retry: 0` forced a manual
  retry every time (jobs 192219/192276/192388/192371 on 2026-07-18, all green
  on retry). The job now retries once on `script_failure` in addition to the
  infra failure reasons, so MWPS survives the flake without babysitting.
