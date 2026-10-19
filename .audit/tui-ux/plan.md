# Terminal UX phase plan

The goal is a human-operated retained-child panel in released PiG, with compact tool cards and a responsive widget. Model tools retain their schemas and raw JSON. Native sessions remain the lifecycle authority.

| Phase | Result | Verification |
| --- | --- | --- |
| Prove the public SDK interaction | Complete | Released-host component, width callback, context lifetime and teardown probes. |
| Render compact tool cards | Complete, `7736a46` | All six operations, expanded details, pagination, terminal safety, Unicode and native Ctrl+O. |
| Keep active work visible | Complete, `1df7b24` | Older active children, recent settled rows, 32/80/100 columns, owner retirement overlap and a 20,000-record projection. |
| Add retained child controls | Complete, `4dc348b` | Inspect, latest report, message, subtree pause, explicit same-UUID resume and per-run observation through the existing Tree. |
| Prove the complete product | Complete, `4703fc3` and `3baeb7e` | Direct/Piglet/unselected/control matrix, independent normal Piglet run, full Go checks, factory validation and helper fault tests. |
| Review and deliver | Current | Commit the evidence briefing, independently review the exact transcript suffix, and open a ready PR. Do not merge. |

Product phases serialize because they share the adapter and Tree. Each phase has an independent read-only judge. Source changes and terminal proof use separate worktrees with explicit ownership. Final native runs use private fixtures and at most two simultaneous cases per worker.

The acceptance conditions are concrete.

- Passive panel use starts no inference, changes no pause state and consumes no completion receipt.
- Explicit resume authorizes only the selected direct child. A current worker can later authorize its own child through its existing controls.
- Native UUIDs, accepted input history, outcomes and receipts survive reload and restart. Cancelled drafts contribute zero native messages.
- Actual saved frames contain every control at 32, 80 and 100 columns. Fixed-fixture actions stay below two seconds.
- Default Go tests, race tests, vet, builds, generated Resource and standalone registration, and Piglet validation pass.
- All product behavior stays inside the selected Go Resource through the newest released public PiG and SDK APIs. No host fork, fake acknowledgement, queue imitation, synthetic history or product companion is used.

The [result map](README.md), [native summary](native-matrix.json), [independent verdicts](reviews.md) and [append-only trail](../tui-ux.tsv) retain the outcomes and corrections. Large local protocol records remain outside Git.
