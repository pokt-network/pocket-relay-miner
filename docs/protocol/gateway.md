## Gateway — delegation and the signing ring

Verified against **poktroll v0.1.35** (`go.mod:19`). **Without a citation, it is not a rule.**

### The fields, and there are few

`poktroll/x/gateway/types`, `type Gateway struct`: `address`, `stake`,
`unstake_session_end_height`, `metadata`.

**There is no list of applications.** The relationship is stored by the APPLICATION,
not the gateway: `delegatee_gateway_addresses` and `pending_undelegations` live in
`poktroll/x/application/types` (see `docs/protocol/application.md`).

**Consequence**: to know which applications a gateway can serve, **you do not query
the gateway**. You query the applications.

### Rule 1 — the FIVE events

`poktroll/x/gateway/types/event.pb.go`: `EventGatewayStaked`,
`EventGatewayUnbondingBegin`, `EventGatewayUnbondingEnd`,
`EventGatewayUnbondingCanceled`, `EventGatewayMetadataUpdated`.

**`EventGatewayUnbondingCanceled` exists**: same as in supplier and in
application, **unbonding is reversible and not terminal**. The three entities
share that shape.

**There is no delegation event here.** Delegation is observed from the application
side: `EventRedelegation` (see `application.md`, rule 3).

### Rule 2 — the gateway is staked too, and can unstake too

It has `stake` and `unstake_session_end_height`, with the same shape as supplier and
application. An unstaking gateway **does not stop existing all at once**.

### What this document does NOT cover yet

**The construction of the signing ring** — which keys go in, in what order, and what
happens with a delegation that changes mid-session. This repo verifies ring
signatures in `rings/` (copied from poktroll), and the mechanics of the wire format
are measured separately, but **the protocol rules on when a delegation starts and
stops being valid were not read for this document**.

That is the question that matters: if an application delegates or undelegates
mid-session, **which ring is the valid one for a relay already served**. Before
claiming anything about that, `x/application/keeper` and the ring assembly have to be
read, with the same method: cite it or it does not go in.
