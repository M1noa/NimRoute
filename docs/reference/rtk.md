# RTK — tool_result compression

Port of `rtk-ai/rtk`. Compresses tool output (git status/diff/log, grep,
find, ls, build logs) inside the request before it goes upstream, so the
model reads a compact version instead of the raw dump. Measured 70-90%
smaller on those payloads.

- off by default (`rtk.enabled`, `config.yml.example` §rtk): it trades
  output fidelity for input tokens, so it is opt-in.
- error results are never touched: failure traces stay intact.
- `rtk.blind_truncate`: last resort, cuts the middle of any >250-line blob
  no filter recognizes. off = only positively-identified formats compress.
- code: `rtk.go` (format filters), `rtkcompress.go` (body walk +
  `rtkCompressBody` entry), `rtkdetect.go` (format detection). wired in
  `anthropic.go` on the request path.
- `setTextParts` shape-guards every block: malformed `text` blocks are
  left untouched, never panicked (see `FuzzRtkCompressBody` seeds).
