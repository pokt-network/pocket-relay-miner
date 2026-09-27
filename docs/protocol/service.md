## Service — cuánto vale un relay, y cuáles entran al árbol

Verificado contra **poktroll v0.1.35** (`go.mod:19`). **Sin cita, no es una regla.**

El service es la entidad que decide **el precio de un relay** y **qué fracción de
los relays servidos llega a ser reclamable**. Es donde vive el multiplicador que
convierte hojas del árbol en plata, así que casi toda afirmación de la forma
"cobramos menos de lo que servimos" se resuelve acá.

### Los campos

`poktroll/x/shared/types/service.pb.go`, `type Service struct`:

| campo | |
|---|---|
| `id` | |
| `name` | |
| `compute_units_per_relay` | **CUPR**: cuántas unidades de cómputo vale UN relay de este servicio |
| `owner_address` | |
| `metadata` | |

### Regla 1 — el CUPR es del SERVICIO, no del relay ni del supplier

Un relay no lleva su precio: lo hereda del service. **Cambiar el CUPR cambia el
valor de todos los relays de ese servicio**, y lo hace para cualquier sesión que
se liquide después del cambio.

### Regla 2 — la dificultad decide qué relay ENTRA al árbol

`poktroll/x/service/types/relay_mining_difficulty.pb.go`, `type
RelayMiningDifficulty struct`:

| campo | |
|---|---|
| `service_id` | |
| `block_height` | |
| `num_relays_ema` | la media móvil exponencial de relays del servicio |
| `target_hash` | **el umbral**: sólo entran al árbol los relays cuyo hash lo cumple |

Ver `docs/protocol/claim-and-proof.md`, regla 3: *"not every Relay (Request,
Response) pair in the session is inserted into the tree. The relay hash has to
have matched the difficulty for that service."*

### Regla 3 — la fórmula del multiplicador, exacta

`poktroll/pkg/crypto/protocol/relay_difficulty.go:85-96`:

```
probability = target_hash / BaseRelayDifficultyHash      (GetRelayDifficultyProbability)
multiplier  = 1 / probability                            (GetRelayDifficultyMultiplier)
```

Textual del código: el multiplicador *"scales FROM 'onchain_volume_applicable_relays'
TO 'offchain_estimate_actual_relays'"*.

**Consecuencias:**

- Cuando `target_hash == BaseRelayDifficultyHash`, la probabilidad es 1 y el
  multiplicador es 1: **entra todo, y estimado == reclamado**. Ése es el caso de
  los gates locales, y por eso ahí `servido == facturado` exacto es la aserción
  correcta.
- Con dificultad mayor que la base, **estimado > reclamado por diseño**, y la
  diferencia **no es pérdida**.
- `GetRelayDifficultyMultiplierToFloat32` existe pero el código dice, en
  mayúsculas, *"THIS IS TO BE USED FOR TELEMETRY PURPOSES ONLY"*: la conversión a
  float32 pierde precisión y **no debe usarse para plata**. Lo que se usa es
  `*big.Rat`.

### Regla 4 — estimado = multiplicador × reclamado, y el service ID debe coincidir

`poktroll/x/proof/types/claim.go`, `getNumEstimatedComputeUnitsRat`:

```
numEstimatedComputeUnits = GetRelayDifficultyMultiplier(difficulty.target_hash)
                         × claim.GetNumClaimedComputeUnits()
```

Y antes de multiplicar valida que **el service ID del claim coincida con el de la
dificultad**, o falla con `ErrProofInvalidRelayDifficulty`.

**Consecuencia**: la dificultad usada es la **del servicio del claim**. Aplicar la
dificultad de otro servicio no da un número malo: da un error.

### Regla 5 — la dificultad CAMBIA, y hay un evento que lo dice con ambos valores

`poktroll/x/service/types/event.pb.go`: `EventRelayMiningDifficultyUpdated`, con
`service_id`, `prev_target_hash_hex_encoded`, `new_target_hash_hex_encoded`,
`prev_num_relays_ema` y `new_num_relays_ema`.

**Es el único evento del módulo service.** Lleva el valor anterior y el nuevo, así
que un cambio de dificultad **se puede fechar y cuantificar desde la cadena** en
vez de inferirse.

### Lo que no se puede leer del código

El CUPR de un servicio concreto y el `target_hash` vigente: son estado de la
cadena. `BaseRelayDifficultyHashBz` sí es una constante del protocolo.
