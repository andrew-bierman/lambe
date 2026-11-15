## Agent-Native Planning Assessment

Project: lambe — supervisor daemon for autonomous coding agents.
Question: should the plan treat agents as first-class users of lambe's own
surface? Answer: **yes, trivially and centrally — lambe's primary users ARE
agents.** This is the rare case where "agent-native planning" is not a lens
applied to a human product; it is the product. The assessment therefore shifts
from "should we accommodate agents?" to classifying which agent-facing actions
get first-class treatment **Now / Later / Never**, and flagging where
agent-as-user creates design obligations a human-oriented plan would miss.

### Applicability

- **Fully applicable, with a twist.** lambe has three user classes, and the
  supervised agent is the most important one:
  1. **Supervised agents** (Codex/Claude/Pi/OpenCode instances) — read
     WORKFLOW.md rendered prompts, write progress to Beads, get gated on
     completion. Every design decision in slice one touches them.
  2. **The daemon's own agent-facing surface** — WORKFLOW.md is a dual-audience
     document (agents execute it; the daemon loads/watches/reloads it — spec
     §6.2 makes dynamic reload REQUIRED, and invalid reloads MUST NOT crash).
     Dual-audience docs are an agent-native concern humans don't have: an
     agent editing WORKFLOW.md mid-run must not take down its own supervisor.
  3. **Future MCP/Rex/ACP consumers** — other agents steering lambe (accept
     handle → return → status/cancel; Rex OSC 7501). Parity questions deferred
     but the plan must not foreclose them.
- **Where the agent-native frame changes real decisions:**
  - **Context parity (Now).** Agents should observe leases, attempts, progress
     through the same `bd` surface humans use — this is already the
     architecture (Beads as entire substrate, progress comments on the hub).
     The plan should make parity an explicit invariant: no lambe-private state
     channel that agents can't read with stock `bd`. The local
     embedded-Beads tracker is the one risk area — session bindings, attempt
     counters, wake timers live locally; the plan must specify which of these
     agents need to see (attempt number is rendered into the prompt per spec
     strict templating `{issue, attempt}`) and confirm they're readable via
     the tracker, not daemon-only memory.
  - **The completion gate is an approval-trust boundary (Now).** "done =
     external checks green, never exit code" exists precisely because agents
     self-report done unreliably (fake-done-with-failing-tests is a
     slice-one fault-injection scenario). Agent-native reading: lambe's core
     product is *distrusting its primary user constructively*. The gate must
     be adversarially robust against an agent that tries to satisfy it
     cheaply (e.g., a gate command the agent itself can edit — see Open
     Questions). Gates defined in hub formulas (git-versioned TOML) are
     outside the agent's workspace; that's the right default and the plan
     should say so.
  - **Prompt contract is an API (Now).** WORKFLOW.md rendering, continuation
     guidance on the same thread, attempt counters, and re-fetch between
     turns — this is the agent-facing API of slice one. Changes to it are
     breaking changes to a non-human client that can't adapt mid-run. The
     plan should pin prompt-contract stability and version WORKFLOW.md front
     matter.
  - **Failover seen from the agent's side (Now, slice one scope).**
     resume-by-session-id vs reconstruct-from-Beads-record is an
     agent-context-preservation decision. The plan should specify what the
     reconstructed context contains (attempt count, progress comments, prior
     gate failures) so a taken-over agent doesn't repeat work or re-hit the
     same failing gate blind.
  - **Surfaces parity (Later, structural guardrail Now).** MCP/Rex/ACP are out
     of scope, but the adapter-seam rule already reserves them. One cheap
     Now item: define the daemon's internal control vocabulary
     (spawn/status/cancel/defer/wake) once so every future surface maps to
     it, rather than letting MCP's handle pattern and Rex's labels drift into
     different state words for the same facts.

### Planning Changes (concrete inputs)

Classification of agent-facing actions:

**Now (slice one):**
1. **Context-parity invariant**: write it into the plan as a testable
   requirement — every piece of state a supervised agent needs to act
   (attempt number, lease/claim state, progress history, gate results,
   deferral reason/wake time) is readable through `bd` (hub or local tracker)
   with no lambe-CLI-only channel. Fault-injection suite gains a "what does
   the agent see?" assertion per scenario.
2. **Gate trust boundary**: gates live in hub formulas
   (`.beads/formulas/`, git-versioned), not in the agent's sanitized
   workspace; gate results written back as Beads state (progress comment or
   issue field) so the *next* attempt's agent reads why the last one was
   rejected. Fake-done rejection is only useful if the agent learns why.
3. **Prompt contract pinned**: WORKFLOW.md front matter schema + `{issue,
   attempt}` rendering treated as a versioned API; continuation-guidance
   content specified; WORKFLOW.md hot-reload must not crash the daemon (spec
   REQUIRED) and must not leave an in-flight run on a half-applied workflow —
   add an explicit reconcile-on-reload rule.
4. **Reconstruct-context spec**: enumerate what a cross-machine takeover
   prompt contains from the Beads record, so reconstruction is designed, not
   emergent.
5. **Approval posture documented** (spec SP1071-1072 REQUIRES each impl to
   document it): approvals/user-input MUST NOT stall a run indefinitely —
   lambe runs unattended, so the posture is "no interactive approvals; agent
   proceeds or defers via state." One paragraph in the plan discharges a
   conformance requirement.

**Later (post-slice-one, keep seams only):**
6. MCP accept-handle-return surface, Rex OSC 7501, ACP driver, HTTP state —
   all as planned. Add one guardrail: a shared control-vocabulary note so
   surfaces present identical facts (a status via MCP == a status via Rex ==
   `bd` read).
7. Agent-steering-agent trust: when an MCP client agent can cancel/steer
   other agents, that's a new trust boundary (prompt-injection risk flagged
   in deep-mcp-subagents). Note it in the plan as a Later design item; do not
   design now.

**Never:**
8. No agent-self-reported completion path, ever — exit-code or agent-asserted
   done must remain non-terminal. (Already settled; this confirms it as an
   agent-trust decision, not just an engineering one.)
9. No lambe-owned agent context plane beyond Beads (no private agent-memory
   side channel; consistent with the no-own-state-plane non-goal).

### Open Questions

1. **Can a supervised agent modify its own gates?** Gates in hub formulas are
   outside the workspace, but the gate *commands* often run `gh:run`/CI on
   the agent's own PR — an agent could weaken tests in the same commit that
   claims done. Does slice one treat "gate executes against pre-agent-change
   ref / CI result on the PR" or is same-commit gaming accepted for now?
   Recommend: document the boundary explicitly; cheap mitigation is running
   gates on the committed ref via CI status rather than locally.
2. **Local-tracker readability**: is the embedded Beads local tracker
   readable by the supervised agent in-workspace (for attempt/deferral
   context), or daemon-only? If daemon-only, attempt number in the prompt may
   be the only parity channel in slice one — acceptable, but say so.
3. **WORKFLOW.md authorship**: who edits it — the human, or also agents
   (improving their own workflow)? If agents may edit, reload semantics
   become an agent-trust boundary too (an agent could edit hooks to disable
   gates). Recommend: agents get read+execute, edits are human-only (git
   review on the hub repo covers it).
4. **Continuation-guidance content**: spec mandates guidance-only turns; is
   guidance generated by lambe (template) or read from Beads state? Small,
   but it's the highest-frequency agent-facing text in the system.

**Standing-line check**: no evidence any settled decision fails from the
agent-native angle. If anything, agent-native analysis *strengthens* two of
them: Beads-as-entire-substrate is what makes context parity nearly free, and
the verified-completion gate is exactly the right agent-distrust mechanism.
