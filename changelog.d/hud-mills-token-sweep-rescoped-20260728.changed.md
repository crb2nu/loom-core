HUD Mills: finish the semantic token sweep in `tokens.css` and `PanelShell`.
`--mills-radius-round` now resolves through `--radius-full` instead of a
literal `50%` (identical rendering — both consumers are fixed squares, 58×58
and 56×56), and `--mills-text-heading` clamps between `--text-lg` and
`--text-xl` instead of literal `18px`/`24px`.

Note one intentional visual change: the type scale steps 18 → 22 → 30px, so
the heading's upper bound snaps from 24px to `--text-xl` (22px). Panel
headings (`PanelShell.svelte`, the sole consumer) cap 2px smaller at wide
viewports; the lower bound is unchanged at 18px.
