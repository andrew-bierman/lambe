# External evidence: Symphony spec ecosystem & multi-orchestrator precedent
Compiled 2026-11-15. Reference impl (openai/symphony Elixir) and ahkohd/symphony-beads excluded per brief.

## Slice A — implementations & derivatives
- ryanjdillon/symphony — Go, "conforms to the Symphony SPEC", polls Linear, spawns Claude Code/Codex, retries/concurrency/reconciliation. 1 star, last push 2026-04-26. Single orchestrator, in-memory. [verified: https://github.com/ryanjdillon/symphony] [single-source]
- hojinzs/github-symphony — TypeScript, GitHub Projects tracker + multi-workspace, ".gh-symphony-start.lock identity lease in the canonical project folder", "A single orchestrator manages multiple projects". 17 stars, last push 2026-10-29. Most active derivative. [verified: https://github.com/hojinzs/github-symphony]
- moabualruz/fulcrum — TypeScript "Local-first Agent OS" on PGlite; "fulcrum doctor validates conformance to the openai/symphony orchestration spec". 6 stars, push 2026-10-02. [verified: https://github.com/moabualruz/fulcrum]
- Uddoo/dsh-dashboard — TypeScript "Symphony-compatible multi-provider task orchestrator" (Linear/GitHub/Jira/Asana/GitLab/local) for DeepSeek Harness; also listed in dshdocs plugin directory. 3 stars, push 2026-08-15. [verified: https://github.com/Uddoo/dsh-dashboard]
- NoWint/Oh-My-DSH — DSH plugin "Symphony-compatible Linear issue orchestrator". [verified: https://github.com/NoWint/Oh-My-DSH] [single-source]
- paperclipai/paperclip — TypeScript, 114k stars, push 2026-11-15 (very active). docs/specs/external-task-protocol.md: boundary "inherited from OpenAI Symphony (Section 3.3)"; plans "Reuse Paperclip's existing execution semantics ... as a Symphony-compatible orchestration profile". Largest project engaging the spec; adoption is design-doc level, not full impl. [verified: https://github.com/paperclipai/paperclip/blob/master/docs/specs/external-task-protocol.md]
- OpenSymphony — author's implementation of the Symphony design using Linear; write-up only. [doc-claim: https://trilogyai.substack.com/p/from-spec-driven-work-to-work-orchestration] [single-source]
- No ports under the openai org; no Go/Rust/Python official impls found. Absence-of-findings after GitHub/press search.

## Slice A — spec trajectory
- SPEC.md actively maintained: commits 2026-03-04 → 2026-08-12 (GitHub API, path=SPEC.md). Notable: 2026-03-11 "Add SSH worker support" (remote/distributed *workers*, per-host concurrency caps); 2026-04-27 RFC-2119 conformance-language pass (#61); 2026-07-18 "Add generic tracker interface with Linear adapter" (#102) — spec de-Linearized, tracker becomes pluggable boundary; 2026-08-12 credential-scrubbing security fix (#119). [impl-verified: https://api.github.com/repos/openai/symphony/commits?path=SPEC.md]
- Press pickup: OpenAI announcement 2026-04-27 (openai.com/index/open-source-codex-orchestration-symphony), InfoWorld, InfoQ (2026-05), tessl, Stack Futures; HN thread id=47252045; r/codex thread. [doc-claim, multiple sources]
- Ecosystem signal: ≥6 independent "symphony-compatible" projects across Go/TS + one design-doc adoption by a 114k-star project; no formal RFC process, no interop test suite, no spec discussions venue found beyond repo issues. Direction of travel is real but shallow — most derivatives have <20 stars. [inference from above]

## Slice B — multi-orchestrator precedent
- SPEC.md line 51: service MUST "Maintain a single authoritative orchestrator state for dispatch, retries, and reconciliation." [verified: https://raw.githubusercontent.com/openai/symphony/main/SPEC.md]
- §4.1.8 Orchestrator Runtime State: "Single authoritative in-memory state owned by the orchestrator" — claims/running/retry state is in-memory, per-process; no persistence, no lease/TTL/lock semantics anywhere in spec (grep for lease/lock: zero hits). [verified: same URL]
- Line 727: "The orchestrator serializes state mutations through one authority to avoid duplicate dispatch." Claims are orchestrator-local, not tracker-visible leases — two orchestrators on one tracker would race with nothing in the spec to stop duplicate dispatch. [impl-verified: same URL]
- Explicit non-goal (line 62): "Rich web UI or multi-tenant control plane." Blog corroboration: "The spec explicitly lists a rich multi-tenant control plane as a non-goal; this is deliberately a single-orchestrator tool." [verified: https://dreaming.press/posts/openai-symphony-issue-tracker-coding-agent-control-plane.html]
- openai/symphony issues search for "multiple orchestrators": 0 results; "multi orchestrator": 1 unrelated SSH-retry fix. No maintainer discussion of the pattern. [verified: https://api.github.com/search/issues?q=repo:openai/symphony+multiple+orchestrators]
- github-symphony: uses a local lock-file "identity lease" per project, but explicitly "A single orchestrator manages multiple projects" — one orchestrator, many projects; not multi-orchestrator/one-tracker. [verified: https://github.com/hojinzs/github-symphony]
- One user report of co-locating multiple orchestrator families (Codex, Claude, "Symphony-family") on one board via `exec:agent` / `exec:symphony` ownership labels + `blockedBy` — coordination by tracker convention, not by spec. [user-report: https://dev.to/mscalessio/i-tried-to-run-symphony-for-real-on-rentello-it-broke-in-exactly-the-right-place-j4a] [single-source]

## Verdict on Slice B
Multi-orchestrator against one shared tracker is a **spec divergence**: the spec mandates one authoritative orchestrator with in-memory claim state and provides no leasing/locking/dedup across processes. The single user report achieves coexistence via tracker-label conventions, outside the spec. Durability is likewise unspecified (in-memory state is normative). [inference, grounded in spec text above]
