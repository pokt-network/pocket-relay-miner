## Session — las reglas del protocolo

Verificado contra **poktroll v0.1.35** (`go.mod:19`). Cada regla cita su fuente.
**Sin cita, no es una regla.**

Una sesión es la unidad de contabilidad: los relays se agrupan por sesión, se
reclaman por sesión y se liquidan por sesión. Todas las alturas de abajo salen de
`poktroll/x/shared/types/session.go`.

### La línea de tiempo, con las fórmulas exactas

```
sessionEnd
  claimWindowOpen  = sessionEnd       + ClaimWindowOpenOffsetBlocks + 1
  claimWindowClose = claimWindowOpen  + ClaimWindowCloseOffsetBlocks
  proofWindowOpen  = claimWindowClose + ProofWindowOpenOffsetBlocks
  proofWindowClose = proofWindowOpen  + ProofWindowCloseOffsetBlocks
```

`session.go:99-104`, `:109-112`, `:117-119`, `:124-126`.

**El `+ 1` de `claimWindowOpen` es parte de la fórmula**, no un redondeo: la
ventana abre en el bloque SIGUIENTE al offset. Un cálculo que lo omita queda un
bloque adelantado.

Aparte, y son otra cosa:

- `GetSessionGracePeriodEndHeight = sessionEnd + GracePeriodEndOffsetBlocks`
  (`session.go:86-88`).
- `GetSettlementSessionEndHeight` (`session.go:210`) se apoya en
  `GetSessionEndToProofWindowCloseBlocks`.

### Regla 1 — NO hay reparto de claims entre suppliers. Está comentado.

`session.go`, `GetEarliestSupplierClaimCommitHeight`: la función recibe el hash
del bloque de apertura y la dirección del supplier **para sortear un offset
determinista**, y **todo ese cuerpo está comentado**. Devuelve
`claimWindowOpenHeight` pelado.

Lo mismo en `GetEarliestSupplierProofCommitHeight`: devuelve
`proofWindowOpenHeight` pelado.

El propio código lo explica: *"Having proof distribution windows was a
requirement that was never determined to be necessary, but implemented regardless.
We are keeping around the functions but TBD whether it is deemed necessary."*

**Consecuencias, y son operativas:**

- **Todos los suppliers pueden presentar en la MISMA altura.** No hay spread que
  lo evite. Cualquier suposición de "la cadena reparte la carga de submissions"
  es falsa hoy.
- El nombre de la función (`EarliestSupplier...`) **sugiere** un reparto por
  supplier que no ocurre. Leer el nombre y no el cuerpo lleva a la conclusión
  contraria.
- Si este repo espacia sus envíos, **el espaciado es nuestro**, no del protocolo,
  y no hay que atribuírselo a la cadena.

### Regla 2 — las ventanas se calculan desde `queryHeight`, no desde "ahora"

Todas las funciones toman `queryHeight` y derivan el fin de sesión con
`GetSessionEndHeight(sharedParams, queryHeight)`. Junto con la Regla 3, esto es lo
que hace que una sesión pasada se pueda recalcular exactamente.

### Regla 3 — los parámetros son HISTÓRICOS

`poktroll/x/session/keeper/session_hydrator.go:190` lee
`k.GetParamsAtHeight(ctx, sh.blockHeight)`, con el comentario *"Use historical
params to ensure deterministic session hydration for historical heights"*.

**Consecuencia**: usar los parámetros de HOY para razonar sobre una sesión vieja
da un resultado equivocado si algún parámetro cambió. Los offsets de arriba se
evalúan con los parámetros de la altura de esa sesión.

### Regla 4 — quién entra en la sesión

Ver `docs/protocol/supplier.md`, regla 6: los candidatos salen **sólo** de las
configuraciones de servicio activas a esa altura; el unbonding no se consulta. Si
no hay candidatos, la hidratación **falla** con `ErrSessionSuppliersNotFound` — no
devuelve una sesión vacía.

### Lo que no se puede leer del código

Los VALORES de mainnet de `ClaimWindowOpenOffsetBlocks`,
`ClaimWindowCloseOffsetBlocks`, `ProofWindowOpenOffsetBlocks`,
`ProofWindowCloseOffsetBlocks`, `GracePeriodEndOffsetBlocks`,
`NumBlocksPerSession` y `NumSuppliersPerSession`. Son estado de la cadena. Los
defaults del código no son evidencia de lo que corre en mainnet.
