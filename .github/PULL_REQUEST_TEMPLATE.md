**Summary**
What this PR does and why.

**Units touched**
Packages/modules changed (e.g. `internal/symphony`, `internal/daemon`).

**Test evidence**
Which suite was run and with what flags (expect `LAMBE_TEST_BD=1` where
tracker code is touched, `-race` where practical):

```
paste command + result summary
```

**Symphony-spec impact**
State “None” when behavior is internal to Lambe. Otherwise, cite the
SPEC.md sections and describe the conformance-relevant behavior change.

**Checklist**
- [ ] Docs updated (or N/A).
- [ ] `CHANGELOG.md` updated under Unreleased.
- [ ] `gofumpt`, `golangci-lint`, and `staticcheck` pass.
