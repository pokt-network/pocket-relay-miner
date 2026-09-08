## Parámetros de governance — el inventario completo, y qué nos hace cada uno

Verificado contra **poktroll v0.1.35** (`go.mod:19`), enumerando los `Params` de
cada módulo desde la fuente. **Sin cita, no es una regla.**

**Todos son de governance**: cambian por propuesta, sin que toquemos código, y
varios cambian **cuánto cobramos**. Los defaults de abajo son **del código**, no
de mainnet — el valor vigente se consulta a la cadena.

### `x/shared` — 13 parámetros, y acá vive el precio

| parámetro | qué nos hace |
|---|---|
| `compute_units_to_tokens_multiplier` | **el precio**. Default `42_000_000` (`params.go:29`) |
| `compute_unit_cost_granularity` | el divisor del precio: el par CUTTM/granularidad da el uPOKT de UNA unidad de cómputo |
| `num_blocks_per_session` | el largo de la sesión; entra en la fórmula del fin de unbonding |
| `claim_window_open_offset_blocks` | cuándo podemos reclamar |
| `claim_window_close_offset_blocks` | hasta cuándo |
| `proof_window_open_offset_blocks` | cuándo podemos probar |
| `proof_window_close_offset_blocks` | hasta cuándo. Pasado esto el claim expira y **cuesta stake** |
| `grace_period_end_offset_blocks` | el período de gracia de la sesión |
| `supplier_unbonding_period_sessions` | cuánto tarda en salir un supplier nuestro |
| `application_unbonding_period_sessions` | ídem para quien paga |
| `gateway_unbonding_period_sessions` | ídem gateway |
| `session_grid_anchor_height` | el ancla del numerado de sesiones |
| `session_number_at_anchor` | ídem |

**Los cuatro offsets de ventana son la agenda entera del miner.** Un cambio de
governance ahí mueve cuándo hay que reclamar y probar; ver `session.md`.

### `x/session` — 1

| parámetro | qué nos hace |
|---|---|
| `num_suppliers_per_session` | cuántos suppliers entran por sesión. **Se lee con parámetros HISTÓRICOS** (`session_hydrator.go:190`), así que una sesión vieja se rehidrata con el valor de ESA altura |

### `x/service` — 2

| parámetro | qué nos hace |
|---|---|
| `target_num_relays` | el objetivo al que la EMA de dificultad converge: **mueve el `target_hash` y por lo tanto cuántos de nuestros relays entran al árbol** |
| `add_service_fee` | costo de dar de alta un servicio |

### `x/supplier` — 2

| parámetro | qué nos hace |
|---|---|
| `min_stake` | por debajo, la cadena **nos baja sola**: `SUPPLIER_UNBONDING_REASON_BELOW_MIN_STAKE` (ver `supplier.md`) |
| `staking_fee` | se paga al stakear |

### `x/application` — 2

| parámetro | qué nos hace |
|---|---|
| `min_stake` | el piso de quien paga |
| `max_delegated_gateways` | **cota del tamaño del anillo de firma** |

### `x/gateway` — 1

`min_stake`.

### `x/proof` — 4

| parámetro | qué nos hace |
|---|---|
| `proof_requirement_threshold` | por encima, el proof es **obligatorio** |
| `proof_request_probability` | por debajo del umbral, se **sortea** |
| `proof_missing_penalty` | lo que cuesta no tenerlo |
| `proof_submission_fee` | lo que cuesta enviarlo |

**Los dos primeros son la razón de que un claim sin proof pueda estar bien.**

### `x/tokenomics` — 6, y deciden cuánto nos llega

| parámetro | qué nos hace |
|---|---|
| `mint_allocation_percentages` | **el reparto**. Default: supplier **0.7**, source owner 0.15, DAO 0.1, proposer 0.05, application 0.0 (`x/tokenomics/types/params.go`) |
| `mint_equals_burn_claim_distribution` | el mismo reparto para el régimen mint==burn. Mismos defaults |
| `global_inflation_per_claim` | default `0.1` |
| `mint_ratio` | default `1.0` — *"no deflation (mint equals burn)"* |
| `overservicing_bonus_multiplier` | default `1`. **El cero se trata como 1, nunca como ilimitado** (ver `application.md`) |
| `dao_reward_address` | a dónde va la parte del DAO |

**La regla que más se olvida y está acá**: de lo que se distribuye, **al supplier
le toca 0.7, no 1.0**. Cobrar el 70% no es una pérdida: es el reparto.
