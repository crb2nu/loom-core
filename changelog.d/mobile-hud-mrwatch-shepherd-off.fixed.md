- **mrwatch shepherd switched off** (`k8s/base/servers/mobile-hud/deployment.yaml`):
  it creates a branch pipeline whenever GitLab reports no MR head pipeline,
  but GitLab reports none for MRs whose pipelines were minted via the API
  (the merge queue's recovery pipelines), so on 2026-09-10 it re-minted
  ~150 job-minute pipelines for eight Mills MRs on every poll and doubled the
  CI queue; its arm action also enqueued duplicate merge-queue candidates.
  Off until `bl-mills-pipeline-adopt-before-mint-20260910` lands.
