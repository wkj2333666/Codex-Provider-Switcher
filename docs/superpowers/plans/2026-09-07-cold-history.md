# Cold archived history maintenance

User-approved policy: only archived tasks unused for at least 30 days; preserve active and unarchived tasks and complete history. Use a conservative observation period when historical access is unknown.

- [x] Add regression coverage for canonical JSONL paths whose physical representation is Zstandard, corrupt input, active writers, cancellation and safe publication.
- [x] Resolve compressed siblings and materialize them under native maintenance/writer locks before sanitation; distinguish restoration errors from active-turn errors.
- [x] Record task access in the proxy; add a daily, bounded archive-only worker using SQLite archive metadata, access timestamps and a 30-day observation floor. Verify compression byte-for-byte before publishing and skip changed/locked files.
- [x] Test worker policy, races and integrity on fixtures; test native resume and provider handoff on real history copies.
- [ ] Review, deploy the compatible switcher and install the timer; retain native broad compression disabled.
- [x] Reclaim unused log database pages and obsolete, unused Codex releases; report measured savings and retained data.

Validation: full Go suite and vet passed on the deployment host; 11 Python policy/integrity tests passed. A real 96 MiB compressed archive copy passed unarchive, resume, both provider switches and repeat resume. Deployment and timer installation follow the source commit.
