## Supplier — las reglas del protocolo

Verificado contra **poktroll v0.1.35** (la versión que `go.mod:19` fija). Cada
regla cita el archivo del que sale. **Sin cita, no es una regla**: es una
suposición, y este documento existe porque las suposiciones sobre el protocolo
nos costaron un fin de semana persiguiendo relays que no faltaban.

Las rutas `poktroll/...` son relativas a la raíz del módulo en el module cache.

### Qué es un supplier, y qué campos lo definen

`poktroll/x/shared/types/supplier.pb.go`:

| campo | qué es |
|---|---|
| `operator_address` | quién opera. Es la identidad que firma relays |
| `owner_address` | quién puso la plata y a quién vuelve |
| `stake` | el stake |
| `services` | los servicios declarados |
| `service_config_history` | el historial, con `activation_height` y `deactivation_height` por entrada |
| `unstake_session_end_height` | la sesión en que termina de desmontar |

**El historial no es decoración: es lo que decide si el supplier está activo.**

### Regla 1 — "activo" es POR SERVICIO y POR ALTURA, y NO mira el unbonding

`poktroll/x/shared/types/supplier.go:23-38` — `Supplier.IsActive(queryHeight, serviceId)`
recorre `ServiceConfigHistory` y responde verdadero si hay una entrada para ESE
servicio cuya ventana contiene ESA altura (`activation <= h < deactivation`).

**No consulta `unstake_session_end_height`.** Estar desmontando y estar activo
para un servicio son **ortogonales** en el protocolo.

`IsUnbonding()` es una pregunta distinta y más pobre
(`poktroll/x/shared/types/supplier.go:10-12`): sólo dice si
`unstake_session_end_height != SupplierNotUnstaking`.

**Consecuencia para este repo**: nuestro `cache.SupplierState.IsActive()` es
`Staked && Status != NotStaked` (`cache/supplier_cache.go`), que es MÁS GRUESO que
el del protocolo — no es por servicio ni por altura. Los dos nombres coinciden y
las dos preguntas no. Al afirmar algo sobre "activo", decir cuál de los dos.

### Regla 2 — un supplier que desmonta SIGUE SIRVIENDO

Se deduce de la regla 1: si su configuración de servicio sigue activa a esa
altura, sirve, aunque `IsUnbonding()` sea verdadero.

**Esto ya nos costó un defecto**: el write de drenaje reconstruía un estado
parcial y borraba la vista de transportes de un supplier que seguía sirviendo
(arreglado en `b67b811`).

### Regla 3 — hay CUATRO razones de unbonding, no una

`poktroll/x/supplier/types/event.pb.go`:

- `SUPPLIER_UNBONDING_REASON_VOLUNTARY` — el operador lo pidió
- `SUPPLIER_UNBONDING_REASON_BELOW_MIN_STAKE` — **la cadena lo bajó sola**
- `SUPPLIER_UNBONDING_REASON_MIGRATION`
- `SUPPLIER_UNBONDING_REASON_UNSPECIFIED`

**La segunda es la que se olvida**: un supplier puede entrar en unbonding **sin
que nadie del lado del operador haga nada**, por caer bajo el stake mínimo (por
ejemplo tras un slash). Cualquier razonamiento que asuma "desmontar = alguien
pidió unstake" está incompleto.

### Regla 4 — los SEIS eventos, que es lo que se puede observar

`poktroll/x/supplier/types/event.pb.go`:

| evento | cuándo |
|---|---|
| `EventSupplierStaked` | se stakeó |
| `EventSupplierUnbondingBegin` | empezó a desmontar |
| `EventSupplierUnbondingEnd` | terminó |
| `EventSupplierUnbondingCanceled` | **se canceló** — el unbonding es reversible |
| `EventSupplierServiceConfigActivated` | se activó una config de servicio |
| `EventSupplierStakeStuckInModulePool` | ver regla 5 |

`UnbondingBegin` y `UnbondingEnd` llevan los mismos cuatro campos: `supplier`,
`reason`, `session_end_height`, `unbonding_end_height`.

**`EventSupplierUnbondingCanceled` existe**, así que un unbonding observado NO es
un estado terminal y no se puede tratar como tal.

### Regla 5 — la devolución del stake PUEDE FALLAR, y la plata queda atrapada

`poktroll/x/supplier/keeper/unbond_suppliers.go:88-106`. Si el envío de las
monedas desde el module pool a la cuenta del owner falla, **el supplier se
elimina igual y las monedas se quedan en el module pool**, y se emite
`EventSupplierStakeStuckInModulePool` "for indexer/governance".

Textual del código: *"supplier will be removed and coins will remain in module
pool"*.

**Consecuencia**: "el supplier terminó de desmontar" **no** implica "el owner
cobró". Son dos hechos distintos y hay un evento dedicado justo para el caso en
que divergen.

### Regla 6 — un supplier que DESMONTA SIGUE SIENDO ELEGIDO para sesiones nuevas

`poktroll/x/session/keeper/session_hydrator.go:187-226` — `hydrateSessionSuppliers`
arma los candidatos **sólo** desde el iterador de configuraciones de servicio
(`GetServiceConfigUpdatesIterator(serviceId, blockHeight)`) y se queda con las que
cumplen `IsActive(blockHeight)`.

**No consulta `IsUnbonding()` ni `unstake_session_end_height` en ningún punto.**

Consecuencias, y son de plata:

- A un supplier que está desmontando **se le siguen asignando relays**. Servirlos
  y reclamarlos es correcto; dejar de mantener bien su estado local no lo es.
- Lo que finalmente lo saca de las sesiones **no es el unbonding**: es que al
  terminar se lo ELIMINA del estado (`unbond_suppliers.go`), lo que se lleva sus
  configuraciones de servicio y por lo tanto lo saca del iterador.
- `NumSuppliersPerSession` se lee con `GetParamsAtHeight(ctx, blockHeight)`, o sea
  **parámetros históricos**, para que hidratar una sesión pasada sea determinista.
- Si no hay ni un candidato, la hidratación **falla** con
  `ErrSessionSuppliersNotFound`; no devuelve una sesión vacía.

### Regla 7 — cuándo termina el unbonding, con la fórmula

`poktroll/x/shared/types/supplier.go:75-84`:

```
unbondingEndHeight = unstake_session_end_height
                   + SupplierUnbondingPeriodSessions * NumBlocksPerSession
```

Default de `SupplierUnbondingPeriodSessions`: **1 sesión**
(`poktroll/x/shared/types/params.go:20`). **Es el default del código, NO el valor
de mainnet** — ese hay que consultarlo a la cadena.

La cola de unbonding salta a los que no están desmontando (`unbond_suppliers.go:43`)
y a los que todavía no llegaron a su altura (`:62`).

### Regla 8 — al terminar, la plata puede NO moverse por dos razones distintas

`poktroll/x/supplier/keeper/unbond_suppliers.go:80-106`:

1. **Stake en 0 por slashing** → *no se mueve nada*, y es deliberado: el código
   comprueba `supplier.Stake.IsPositive()` justo para no transferir 0 monedas.
2. **La transferencia falla** (owner que es una module account, estado legacy
   anterior a v0.1.34) → se loguea, se emite `EventSupplierStakeStuckInModulePool`
   y **se continúa a propósito**. Textual: *"Why not halt the chain: pre-existing
   legacy state must not be allowed to brick the EndBlocker"*. El supplier se
   elimina igual y las monedas quedan en el module pool.

O sea: **"terminó de desmontar" no implica "el owner cobró"**, y hay DOS caminos
distintos por los que no cobra.

### Regla 9 — las configuraciones de servicio se activan al ARRANCAR una sesión

`poktroll/x/supplier/keeper/activate_services.go:30-60`. Al empezar una sesión se
recorre `GetActivatedServiceConfigUpdatesIterator(currentHeight)` y se emite un
`EventSupplierServiceConfigActivated` **por cada configuración activada**, con
`ActivationHeight: currentHeight`.

Las entradas de índice huérfanas se **saltean con un log de debug**, no fallan
(`:48-52`).

### Lo único que NO se puede leer del código

El **valor de mainnet** de `supplier_unbonding_period_sessions` y de
`num_blocks_per_session`: son estado de la cadena, no fuente. Se consultan a un
nodo. El default del código (1 sesión) **no** es evidencia de lo que corre en
mainnet.
