# Archive

Historical planning and audit material. Kept because it records why the code
looks the way it does, not because it describes the current state. Nothing
here is maintained.

| File | What it was |
|---|---|
| `PLAN.md` | the lanes, status, freepi, and service-fix plan. All shipped |
| `ANTSEED-PLAN.md` | the original plan for the antseed backend. Shipped |
| `ANTSEED-TODO.md` | antseed tracker. Superseded by the code |
| `FREEBUFF-*.md` | reverse-engineering notes and plans for a freebuff backend that was never implemented in Go. The Python client in `scripts/` still exists |
| `audits/2026-10-07-*.md` | four read-only code audits from 2026-10-07, plus `merged.md` which dedupes them and tracks which items were fixed. Every item in the merged file is closed |

The live work list is [../TODO.md](../TODO.md). The contributor contract is
`AGENTS.md` at the repo root.

`muse` in the merged file's item tags refers to one of the four source
audits. That audit also carries a corrections section: some of the findings
in the other two were verified wrong before anything was fixed against them,
which is worth knowing if you are reading the raw reports.
