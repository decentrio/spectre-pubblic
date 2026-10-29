---
name: spectre-docs
description: Review, fix, or extend the Spectre-Documentation Obsidian vault (Team/Results/Tech-doc/Spectre-Documentation) — the -duc copy rule, code-verified claims with file:line, wikilink integrity, the doc↔code terminology map, benchmark-claim policy, and the HTML→PNG diagram pipeline. Use for any task touching those docs or their diagrams.
---

# Spectre-Documentation vault work

Vault root: `/Users/ducnt/Documents/tmp/Team/Results/Tech-doc/Spectre-Documentation/`. These docs are customer/teammate-facing; the quality bar is that of the code they describe.

## Ground rules

1. **`-duc` copy rule**: before editing any existing note, duplicate it to `<name>-duc.md` and edit the copy; never touch the original — unless the user explicitly says to edit that file in place. New files you create from scratch don't need a copy.
2. **Review ≠ fix**: when asked to "review", deliver findings and stop. Apply fixes only when explicitly asked, and only the flagged items.
3. Files may change under you (Obsidian auto-rewrites links when notes move; the user edits concurrently). On an Edit "file has been modified" error: re-read, re-apply. After any rename/move, re-verify all links into and out of the moved file — Obsidian's auto-rewrite has pointed links at wrong/old paths before.
4. The docs themselves are written in English, regardless of the conversation language.

## Terminology map — doc name ↔ code name

Use the doc-side names consistently in prose; verify claims against the code side.

| Doc term | Code |
|---|---|
| SpectreClient | `contracts/light-clients/spectre/SpectreClient.sol` — owns ALL client state |
| UpdateClient (module) | `contracts/light-clients/spectre/modules/UpdateClient.sol` — pure header checks |
| Membership | `contracts/light-clients/spectre/modules/Membership.sol` — ICS-23, no ZK |
| Misbehaviour | `contracts/light-clients/spectre/modules/Misbehaviour.sol` |
| SignatureVerifier | `contracts/light-clients/spectre/SignatureVerifier.sol` + generated `Groth16Verifier_N{N}.sol` — NOT stateless: holds the owner-set bucket registry (`setBucket`); that is config state, not trust state |
| UpdateConsensusState | `SpectreClient.updateConsensusState` — 24h valset rotation; checks the new valset hash against `header.nextValidatorsHash`, re-pins |
| UpdateApplicationState | `SpectreClient.updateApplicationState` — per-packet appHash advance vs pinned set; still verifies the signature proof, skips re-storing the set |
| Store | consensus-state hash mapping + SSTORE2 pinned valset (≤180 validators) + per-height snapshots (binary-searched) |
| Pinned validator set | the shipped design (PR #198) — never "cache", never "planned" |

Quorum accounting (Σ voting power > ⅔ against the pinned set) happens in **SpectreClient**, not in SignatureVerifier and not in the circuit. The circuit proves input binding (witness hash) + N Ed25519 batch verify; pubkeys are committed to the hash, R/S deliberately are not.

## Claim verification

Every technical statement gets checked against this repo (the `spectre` checkout this skill file lives in) before it survives review:

- Witness/field tables ↔ `witness/witness.go` in the spectre-prover repository (`../prover`) (layout, `MaxMsgLen = 175`, which fields are hashed and why).
- Flow descriptions ↔ `SpectreClient.sol` (`updateApplicationState`, `updateConsensusState`, `_verifyPinnedQuorum`).
- Intervals/constants ↔ relayer source (e.g. the 24h `defaultPeriodicUpdateInterval` in `relayer/relay/module.go`).
- Misbehaviour rules ↔ `contracts/light-clients/spectre/modules/Misbehaviour.sol`.
- Report each verified claim with `file:line` in your findings. Claims you cannot ground: flag as unverified, don't rewrite them into confident prose.

**Benchmark policy**: performance statements stay qualitative ("proves an update in seconds on commodity hardware where SP1 takes minutes"); exact numbers only if produced by a run made this session, quoted with its hardware and sample count. Credit the optimized circuit + gnark stack, not merely "Groth16 instead of zkVM".

## Link integrity

- Footer nav links use the full-path Index form: `[[Team/Results/Tech-doc/Spectre-Documentation/00-introduction/00-introduction|Index]]`. Short wikilinks are ambiguous while older doc sets exist in the vault — prefer full paths for cross-folder links.
- After any review pass, grep every `[[...]]` target and confirm the file exists. Known trap: `02-router.md` intentionally does **not** exist (user deleted it; Components section counts "1 of 2 / 2 of 2") — do not recreate it or link to it.
- External links: `curl -sI` or `gh api` the URL before adding/keeping it. GitHub tree paths change (e.g. IBC_V2 spec dirs may lack a README — link the tree, not a guessed file).
- Content that appears in two notes (e.g. the ChainModule slot table): keep the full version in exactly one note and a one-liner + wikilink in the other.

## Diagram pipeline (HTML → PNG)

Diagrams are hand-written HTML/SVG rendered to PNG with headless Chrome. Sources live outside the vault (repo `diagrams-tmp/` or the session scratchpad); PNGs go to `Tech-doc/attachments/` and are embedded as `![[name.png|<width>]]`.

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --headless=new --disable-gpu --hide-scrollbars \
  --force-device-scale-factor=2 --window-size=<W>,<H> \
  --screenshot=<out>.png file://<abs-path>.html
```

Style rules learned from iteration (violating these caused rework):
- No header/footer text in the image — the note provides the caption.
- Real arrows with arrowheads (`<marker>` defs); for split/merge flows draw explicit trunk+branch paths, never floating unconnected arrows.
- Call-flow diagrams are **mermaid-style sequence diagrams**: participant boxes top *and* bottom, dashed lifelines, solid arrows for calls, dashed open-head arrows for returns, numbered circles at arrow tails, self-calls as small rectangular loops.
- Labels must fit their arrows: widen the canvas before shortening the label; keep labels readable with a background halo (`text { stroke: <bg-color>; stroke-width: 5px; paint-order: stroke; }`).
- Content accuracy beats layout: SignatureVerifier is a sibling called by SpectreClient (not nested under UpdateClient); quorum summing is drawn in SpectreClient; SV is labeled with its registry ("ZK · bucket registry"), never "stateless".
- `--window-size` must match the SVG/body dimensions or the PNG gets padding — set both from the same W×H.

## Findings format

Numbered findings, each with: location (file + line), what's wrong, evidence (code `file:line` or fetched URL status), and the concrete fix. Separate "wrong" from "could be better". End with the list of items needing a user decision versus items that are mechanical.
