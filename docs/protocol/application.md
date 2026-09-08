## Application — las reglas del protocolo, y el presupuesto que recorta el pago

Verificado contra **poktroll v0.1.35** (`go.mod:19`). **Sin cita, no es una regla.**

La application es **quien paga**. Su stake es el presupuesto del que sale lo que
cobra un supplier, y por eso la mayoría de las reglas de "por qué cobré menos de
lo que reclamé" viven acá y no en el claim.

### Los campos

`poktroll/x/application/types`:

| campo | por qué importa |
|---|---|
| `address` | |
| `stake` | el presupuesto |
| `service_configs` y `service_config_history` | igual que en supplier: ventanas por altura |
| `delegatee_gateway_addresses` | los gateways delegados (el anillo de firma) |
| `pending_undelegations` | delegaciones que se están deshaciendo |
| `per_session_spend_limit` | **límite de gasto por sesión** |
| `pending_transfer` | ver regla 3 |
| `unstake_session_end_height` | igual que en supplier |

### Regla 1 — el piso por supplier es B/N, y es un PISO, no un techo

`poktroll/x/tokenomics/keeper/token_logic_modules.go:316-330`, sobre
`ensureClaimAmountLimits`. Textual:

> *"The per-supplier head-split B/N (where B is the application's per-session
> budget and N the actual number of claiming suppliers) is a GUARANTEED FLOOR,
> not a hard ceiling"*

- Servir **en o por debajo** del piso se paga **siempre completo**.
- Servir **por encima** puede cobrarse además del presupuesto que dejaron sin usar
  los suppliers ociosos o livianos, **en proporción al exceso propio**.

**`N` es la cantidad REAL de suppliers que reclamaron**, no la cantidad de
suppliers de la sesión. Un supplier que no reclama agranda el piso de los demás.

### Regla 2 — el bonus por sobreservicio, y el cero que NO significa infinito

`token_logic_modules.go:360-375`:

```
bonus_i = unused * excess_i / totalExcess      (división entera ⇒ Σ bonus ≤ unused)
```

Acotado por `overservicing_bonus_multiplier` (`m`):

- `m == 0` o `m == 1` → tope exactamente en el piso (comportamiento legacy, sin
  redistribución).
- `m > 1` → permite hasta `m * piso` desde el presupuesto no usado.

**El cero se trata deliberadamente como 1, NO como "ilimitado"**, y el código dice
por qué: el valor cero del parámetro —venga de un decode proto3 fresco, de un
handler de upgrade que no corrió, o de una escritura pisada— **debe ser benigno y
nunca habilitar la redistribución en silencio**.

Y la invariante que cierra: `Σ piso + Σ bonus ≤ N*piso = B`, así que **lo
liquidado del grupo nunca supera el presupuesto comprometido y el stake de la
application no puede quedar negativo**.

**Consecuencia para cualquier afirmación de pérdida**: cobrar menos que lo
reclamado **no es un defecto nuestro por defecto**. Puede ser el piso funcionando.
Antes de llamarlo pérdida hay que saber B, N y `m`.

### Regla 3 — una application se puede TRANSFERIR, y falla en tres pasos

`poktroll/x/application/types/event.pb.go` — nueve eventos:
`EventApplicationStaked`, `EventRedelegation`, `EventTransferBegin`,
`EventTransferEnd`, `EventTransferError`, `EventApplicationUnbondingBegin`,
`EventApplicationUnbondingEnd`, `EventApplicationUnbondingCanceled`,
`EventApplicationStakeStuckInModulePool`.

- La transferencia tiene **begin / end / error** y un campo `pending_transfer`:
  no es atómica y **puede fallar**.
- `EventApplicationUnbondingCanceled` existe: igual que en supplier, **el
  unbonding es reversible** y no es terminal.
- `EventApplicationStakeStuckInModulePool` existe: **la misma trampa que en
  supplier** — el stake puede quedar atrapado en el module pool.

### Lo que no se puede leer del código

Los valores de mainnet de `overservicing_bonus_multiplier`, del stake mínimo de
application y del `per_session_spend_limit` de una application concreta. Son
estado de la cadena.
