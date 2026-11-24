---
title: Repository settings for andrew-bierman/lambe (audit log and rollback)
date: 2026-11-23
category: repo-hygiene
module: GitHub repository settings
problem_type: configuration
severity: medium
tags: [github, ruleset, security, dependabot, codeql]
---

# Repository settings

Settings live in GitHub, not in files, so this page is the audit trail. Each row
is before -> after, with the exact `gh api` commands to apply or revert
(`R=repos/andrew-bierman/lambe`). Changed on 2026-11-23.

## Summary

| Setting | Before | After |
| --- | --- | --- |
| Discussions | off | on |
| Merge methods | squash, merge commit, rebase | squash only |
| Squash commit title / message | `COMMIT_OR_PR_TITLE` / `COMMIT_MESSAGES` | `PR_TITLE` / `PR_BODY` |
| Delete branch on merge | off | on |
| Secret scanning | disabled | enabled |
| Secret scanning push protection | disabled | enabled |
| Dependabot alerts (vulnerability alerts) | disabled | enabled |
| Dependabot security updates | disabled | enabled |
| CodeQL code scanning | not configured | default setup, languages `go`, `actions` |
| Private vulnerability reporting | on | on (unchanged) |
| Rulesets / branch protection on master | none | ruleset `master protection` (id 27963635) |
| Labels | GitHub defaults | plus `type:`, `area:`, `priority:` sets and `dependencies` |

## Commands

Apply:

```sh
R=repos/andrew-bierman/lambe
gh api -X PATCH $R -F has_discussions=true
gh api -X PUT $R/vulnerability-alerts
gh api -X PATCH $R --input - <<'JSON'
{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}
JSON
gh api -X PUT $R/automated-security-fixes
gh api -X PATCH $R -F allow_merge_commit=false -F allow_rebase_merge=false \
  -F allow_squash_merge=true -F delete_branch_on_merge=true \
  -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY
gh api -X PATCH $R/code-scanning/default-setup -f state=configured \
  -f 'languages[]=go' -f 'languages[]=actions' -f query_suite=default
gh api -X POST $R/rulesets --input ruleset.json   # ruleset.json is below
```

Revert:

```sh
R=repos/andrew-bierman/lambe
gh api -X PATCH $R -F has_discussions=false
gh api -X DELETE $R/vulnerability-alerts
gh api -X PATCH $R --input - <<'JSON'
{"security_and_analysis":{"secret_scanning":{"status":"disabled"},"secret_scanning_push_protection":{"status":"disabled"}}}
JSON
gh api -X DELETE $R/automated-security-fixes
gh api -X PATCH $R -F allow_merge_commit=true -F allow_rebase_merge=true \
  -F allow_squash_merge=true -F delete_branch_on_merge=false \
  -f squash_merge_commit_title=COMMIT_OR_PR_TITLE -f squash_merge_commit_message=COMMIT_MESSAGES
gh api -X PATCH $R/code-scanning/default-setup -f state=not-configured
gh api -X DELETE $R/rulesets/27963635
```

## The master ruleset

`ruleset.json`:

```json
{
  "name": "master protection",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}],
  "rules": [
    {"type": "deletion"},
    {"type": "non_fast_forward"},
    {"type": "pull_request", "parameters": {
      "required_approving_review_count": 0,
      "dismiss_stale_reviews_on_push": false,
      "require_code_owner_review": false,
      "require_last_push_approval": false,
      "required_review_thread_resolution": false,
      "allowed_merge_methods": ["squash"]}},
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": false,
      "do_not_enforce_on_create": false,
      "required_status_checks": [
        {"context": "check", "integration_id": 15368},
        {"context": "test", "integration_id": 15368}]}}
  ]
}
```

What it does:

- Blocks deleting master and non-fast-forward (force) pushes.
- Requires a pull request with 0 approvals (solo owner) and squash as the only merge method.
- Requires the `check` and `test` status checks (GitHub Actions app, id 15368)
  from `.github/workflows/ci.yml`. Docs-only PRs still run those jobs (the
  Go steps are skipped), so the names always report.
- "Require branches to be up to date" is off (`strict_required_status_checks_policy: false`)
  so merging one PR does not force a rebase of every other open PR.
- **Admin bypass removed (2026-11-23).** The ruleset originally listed repository
  admins (role id 5) as bypass actors with mode `always`. Because every merge
  agent uses the owner's admin token, that meant `check` and `test` were not
  enforced against them, and an audit found two PRs (#45, #55) merged without
  every check on their head commit green. `bypass_actors` is now empty, so the
  required checks and the squash-only rule apply to everyone, including admins.
  Consequence: if CI is down or broken, nothing merges until it is fixed or the
  rule is relaxed. To relax it temporarily, add the bypass actor back:
  `gh api -X PUT repos/andrew-bierman/lambe/rulesets/27963635 --input ruleset.json`
  with `"bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "pull_request"}]`
  (`pull_request` mode limits the bypass to merging through a PR, not direct pushes).

## Could not change

- Secret scanning non-provider patterns and validity checks: the PATCH is
  accepted but the setting stays `disabled` (not available for this repository
  or plan).
- Swift is not in the CodeQL language set; add `swift` to the default-setup
  languages if the LambeDash app should be scanned (uses macOS runners).
- Social preview image: UI-only setting.
- Wiki is still enabled but unused; disable with `-F has_wiki=false` if wanted.
