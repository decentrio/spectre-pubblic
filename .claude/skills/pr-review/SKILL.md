---
name: pr-review
description: Review a Spectre PR, branch, or teammate's changes end-to-end — verify each claimed fix against the linked issue, build + vet + race-test locally, run the relayer path-symmetry and concurrency checks, and deliver a blockers-vs-nits verdict. Use whenever asked to review a PR number, a branch, or a directory of changes.
---

# Spectre PR / branch review

The deliverable is an **assessment**, not fixes. Do not edit code, do not post to GitHub, unless explicitly asked. Deliver the chat verdict in the language of the conversation; anything posted to GitHub is English. Confirm approve-vs-comment before posting a review.

## Protocol

### 1. Establish what you are actually reviewing

```bash
gh pr view <N> --repo decentrio/spectre --json title,body,baseRefName,headRefName,state,additions,deletions,changedFiles,commits
git fetch origin pull/<N>/head:pr-<N>
git merge-base main pr-<N>          # branch may be stale vs main — SHAs in `git log main..` can be rebased duplicates
git show <head-sha> --stat          # when the PR is one commit, review that commit, not a noisy range diff
```

Pitfalls:
- The PR body describes intent; the diff is truth. Review the diff, quote the body only to check claims against it.
- If the PR says `close #<issue>`, open the issue and list its items — you will verify each one individually in step 3.
- Note the working tree state before switching branches (`git status`, current branch name) so you can restore it in step 6.

### 2. Read the full diff, file by file

For each changed file, read the hunks in context — open the surrounding function in the file, not just the diff. Specifically:

- Trace every new lock: what does it protect, who else touches that state, can any path re-enter (recursive helpers, split/retry logic calling back into the locked function)? Grep all callers before declaring "no deadlock".
- Trace every cursor/timestamp/tracker mutation: is it ever advanced on a failure path?
- For removed code: grep the removed symbols — did the removal orphan imports, callers, or tests? Does anything still reference them?

### 3. Verify every claim

Build a table: claimed fix → diff hunk that implements it → verdict (done / partial / missing / does more than claimed). For issue-driven PRs, walk the issue checklist item by item, including items the PR *doesn't* claim — say explicitly which remain open.

Verify semantic claims against the code they depend on, not the diff alone. Example: "now uses `PurgeStaleWithoutTimeout`" is only correct if you read that function's body and confirm its filter semantics.

### 4. Run the machine checks

```bash
git switch pr-<N>
cd relayer && go build ./... && go vet ./...
go test -race -count=1 ./<affected-packages>/...        # macOS ld LC_DYSYMTAB warnings are noise; look for ok/FAIL
# if Solidity touched:
just build-contracts && just lint-solidity
forge test --match-contract <affected> -vvv
forge build --sizes | grep SpectreClient        # if light-client touched — state EIP-170 margin
# if encoding touched:
forge test --match-contract EncodeTest -vvv
gh pr checks <N> --repo decentrio/spectre               # "no checks reported" is itself a finding
```

Quote results verbatim. Local green does not substitute for CI — if CI didn't run, flag it as a merge blocker to resolve.

### 5. Spectre-specific review dimensions

Run these even if the PR doesn't mention them:

- **Path symmetry**: if the change touches one relay direction (Cosmos→ETH or ETH→Cosmos), open the mirror file (paired handler / subscriber / scanner / tracker) and diff behavior. Name the mirror file in the verdict even when it's clean.
- **Crash/restart**: for relayer changes — what state survives a restart? Are recovery scans (startup lookback, periodic gap ticker) still able to cover what the change assumes in-memory?
- **Failure-path state**: no cursor/timestamp/tracker advances when an operation errored; errors logged with context, never swallowed.
- **Goroutine ownership**: for new maps/cursors, state which goroutine owns them. Same-select-loop access needs no lock; anything crossing goroutines does.
- **Witness/encoding boundary**: if `witness/witness.go` in the spectre-prover repository, `SignatureVerifier.sol`, `Encode.sol`, or proto types changed — both sides must change together, and the hashed-field set is a security boundary (demand a binding argument).
- **Generated code**: diffs inside `relayer/bindings/`, `packages/go-abigen/`, `contracts/verifiers/Groth16Verifier_N*.sol` must come from regeneration, matching a source change in the same PR.
- **Reorg/dedup**: for event-handling changes, check dedupe keys include block/tx identity so reorged events re-relay.

### 6. Restore state

```bash
git switch <original-branch>
```

### 7. Deliver the verdict

Structure (in chat):

1. **One-line conclusion** — approve / approve-with-conditions / needs changes, and why.
2. **Claim-by-claim verification** — the table from step 3, with file:line.
3. **Non-blocking suggestions** — nits, edge cases with probability assessment, simplification suggestions.
4. **Meta to fix before merge** — commit message format (repo uses conventional commits; "updates" → squash-merge with `fix(relayer): ...`), `close` vs `refs` when the linked issue is only partially fixed, CI status.
5. Offer (don't do): post the English review to GitHub via `gh pr review` — ask approve vs comment.

Every claim in your verdict must be backed by something you ran or read this session. "Tests pass" means you ran them; "no deadlock" means you grepped the callers.
