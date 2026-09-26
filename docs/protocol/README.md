## The protocol rules

What poktroll does, per entity, and where the money moves. It exists because
reasoning about the protocol from memory missed half the cases and invented
situations that cannot happen.

**Verified against poktroll v0.1.35** — the version pinned by `go.mod:19`.

### The rule of this directory

**Every claim cites the poktroll file it comes from. No citation, no entry.**

And the corollary, which is the hard one: **if it is in poktroll, it gets read.** The
protocol is deterministic and the source is in the module cache; "not verified"
applies only to **chain state** (the parameter values on mainnet), which cannot be
read from the source.

### The documents

| entity | what is most often misread |
|---|---|
| [supplier](SUPPLIER.md) | "active" is per service AND per height, and does not look at unbonding; a supplier that is unbonding **keeps serving and keeps being selected** |
| [session](SESSION.md) | **there is no spreading of claims across suppliers**: that logic is commented out |
| [claim and proof](CLAIM_AND_PROOF.md) | the amount is derived **from the root we signed ourselves**; the proof is **ephemeral** |
| [application](APPLICATION.md) | the `B/N` floor is a **floor, not a ceiling**, so being paid less than claimed is not a loss by default |
| [service](SERVICE.md) | the difficulty decides **which relays enter the tree**; at base difficulty the multiplier is 1 and estimated == claimed |
| [gateway](GATEWAY.md) | the delegation **is stored by the application**, not by the gateway |

### The two cross-cutting ones, and they are the ones that explain the money

| document | what for |
|---|---|
| [params](PARAMS.md) | **the full governance inventory**: 30 parameters in 8 modules, and what each one does to us. They change without us touching code, and several change how much we get paid |
| [interactions](INTERACTIONS.md) | **how everything chains together**: the life of a relay from arrival to the uPOKT the supplier collects, the five legitimate cuts, and which entity answers each question |

### How it is updated

When bumping the poktroll version: re-read the citations, not the prose. A rule whose
citation no longer exists **is not rewritten from memory** — it is read again.
