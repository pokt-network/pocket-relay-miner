## Gateway — la delegación y el anillo de firma

Verificado contra **poktroll v0.1.35** (`go.mod:19`). **Sin cita, no es una regla.**

### Los campos, y son pocos

`poktroll/x/gateway/types`, `type Gateway struct`: `address`, `stake`,
`unstake_session_end_height`, `metadata`.

**No hay lista de applications.** La relación la guarda la APPLICATION, no el
gateway: `delegatee_gateway_addresses` y `pending_undelegations` viven en
`poktroll/x/application/types` (ver `docs/protocol/application.md`).

**Consecuencia**: para saber qué applications puede servir un gateway **no se
consulta el gateway**. Se consultan las applications.

### Regla 1 — los CINCO eventos

`poktroll/x/gateway/types/event.pb.go`: `EventGatewayStaked`,
`EventGatewayUnbondingBegin`, `EventGatewayUnbondingEnd`,
`EventGatewayUnbondingCanceled`, `EventGatewayMetadataUpdated`.

**`EventGatewayUnbondingCanceled` existe**: igual que en supplier y en
application, **el unbonding es reversible y no es terminal**. Las tres entidades
comparten esa forma.

**No hay evento de delegación acá.** La delegación se observa por el lado de la
application: `EventRedelegation` (ver `application.md`, regla 3).

### Regla 2 — el gateway también se stakea, y también puede desmontar

Tiene `stake` y `unstake_session_end_height`, con la misma forma que supplier y
application. Un gateway desmontando **no deja de existir de golpe**.

### Lo que este documento NO cubre todavía

**La construcción del anillo de firma** — qué claves entran, en qué orden, y qué
pasa con una delegación que cambia a mitad de sesión. Este repo verifica firmas de
anillo en `rings/` (copiado de poktroll), y la mecánica del formato de cable está
medida aparte, pero **las reglas del protocolo sobre cuándo una delegación empieza
y deja de valer no se leyeron para este documento**.

Es la pregunta que importa: si una application delega o des-delega a mitad de
sesión, **qué anillo es el válido para un relay ya servido**. Antes de afirmar
nada sobre eso hay que leer `x/application/keeper` y el armado del anillo, con el
mismo método: cita o no entra.
