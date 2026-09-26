## Session — the protocol rules

Verified against **poktroll v0.1.35** (`go.mod:19`). Each rule cites its source.
**Without a citation, it is not a rule.**

A session is the accounting unit: relays are grouped by session, claimed by
session and settled by session. All the heights below come from
`poktroll/x/shared/types/session.go`.

### The timeline, with the exact formulas

```
sessionEnd
  claimWindowOpen  = sessionEnd       + ClaimWindowOpenOffsetBlocks + 1
  claimWindowClose = claimWindowOpen  + ClaimWindowCloseOffsetBlocks
  proofWindowOpen  = claimWindowClose + ProofWindowOpenOffsetBlocks
  proofWindowClose = proofWindowOpen  + ProofWindowCloseOffsetBlocks
```

`session.go:99-104`, `:109-112`, `:117-119`, `:124-126`.

**The `+ 1` in `claimWindowOpen` is part of the formula**, not a rounding: the
window opens at the block AFTER the offset. A calculation that omits it ends up one
block early.

Separately, and these are something else:

- `GetSessionGracePeriodEndHeight = sessionEnd + GracePeriodEndOffsetBlocks`
  (`session.go:86-88`).
- `GetSettlementSessionEndHeight` (`session.go:210`) relies on
  `GetSessionEndToProofWindowCloseBlocks`.

### Rule 1 — there is NO spreading of claims across suppliers. It is commented out.

`session.go`, `GetEarliestSupplierClaimCommitHeight`: the function receives the hash
of the opening block and the supplier's address **to draw a deterministic
offset**, and **that whole body is commented out**. It returns bare
`claimWindowOpenHeight`.

The same in `GetEarliestSupplierProofCommitHeight`: it returns bare
`proofWindowOpenHeight`.

The code itself explains it: *"Having proof distribution windows was a
requirement that was never determined to be necessary, but implemented regardless.
We are keeping around the functions but TBD whether it is deemed necessary."*

**Consequences, and they are operational:**

- **All suppliers can submit at the SAME height.** There is no spread to
  prevent it. Any assumption that "the chain spreads the submission load"
  is false today.
- The function's name (`EarliestSupplier...`) **suggests** a per-supplier spread
  that does not happen. Reading the name and not the body leads to the opposite
  conclusion.
- If this repo spaces out its submissions, **the spacing is ours**, not the protocol's,
  and must not be attributed to the chain.

### Rule 2 — the windows are computed from `queryHeight`, not from "now"

Every function takes `queryHeight` and derives the session end with
`GetSessionEndHeight(sharedParams, queryHeight)`. Together with Rule 3, this is
what makes a past session exactly recomputable.

### Rule 3 — the parameters are HISTORICAL

`poktroll/x/session/keeper/session_hydrator.go:190` reads
`k.GetParamsAtHeight(ctx, sh.blockHeight)`, with the comment *"Use historical
params to ensure deterministic session hydration for historical heights"*.

**Consequence**: using TODAY's parameters to reason about an old session
gives a wrong result if any parameter changed. The offsets above are
evaluated with the parameters at that session's height.

### Rule 4 — who enters the session

See `docs/protocol/supplier.md`, rule 6: candidates come **only** from the
service configurations active at that height; unbonding is not consulted. If
there are no candidates, hydration **fails** with `ErrSessionSuppliersNotFound` — it
does not return an empty session.

### What cannot be read from the code

The mainnet VALUES of `ClaimWindowOpenOffsetBlocks`,
`ClaimWindowCloseOffsetBlocks`, `ProofWindowOpenOffsetBlocks`,
`ProofWindowCloseOffsetBlocks`, `GracePeriodEndOffsetBlocks`,
`NumBlocksPerSession` and `NumSuppliersPerSession`. They are chain state. The
code's defaults are not evidence of what runs on mainnet.
