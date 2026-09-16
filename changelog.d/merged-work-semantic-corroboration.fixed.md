- **Mills council**: semantic merged-work grounding now only corroborates a
  lexically plausible pair (Jaccard in the gray band or above) and can no
  longer ground a proposal on embedding similarity alone. The scorer maps
  cosine as (cos+1)/2, so unrelated titles from the same codebase scored
  0.83–0.90 against some merged MR and every council proposal was dropped as
  "restating recently-merged work" — the mutator-side half of the 50-run dry
  spell that began two days after semantic grounding was activated
  (2026-08-19). Verified on the 2026-09-02 18:21Z run: 10 proposed, 9 kept by
  the editor guardrail, 0 created.
