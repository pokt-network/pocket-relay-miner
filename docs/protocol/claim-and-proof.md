## Claim y Proof — las reglas del protocolo, y por dónde se mueve la plata

Verificado contra **poktroll v0.1.35** (`go.mod:19`). Cada regla cita su fuente.
**Sin cita, no es una regla.**

### Regla 1 — el Claim tiene CUATRO campos, y la cantidad NO es uno de ellos

`poktroll/x/proof/types/types.pb.go`, `type Claim struct`:

| campo | |
|---|---|
| `supplier_operator_address` | quién reclama |
| `session_header` | qué sesión |
| `root_hash` | **el root del árbol que construimos y firmamos NOSOTROS** |
| `proof_validation_status` | ver regla 4 |

No hay `num_relays`. No hay `num_compute_units`. **No hay cantidad guardada.**

### Regla 2 — la cantidad se DERIVA de nuestro root, así que la cadena no es oráculo

`poktroll/x/proof/types/claim.go:20-22` y `:32-34`:

```go
func (claim *Claim) GetNumClaimedComputeUnits() (uint64, error) {
	return smt.MerkleSumRoot(claim.GetRootHash()).Sum()
}
func (claim *Claim) GetNumRelays() (uint64, error) {
	return smt.MerkleSumRoot(claim.GetRootHash()).Count()
}
```

`num_relays` es el **Count** y las unidades reclamadas son el **Sum** del mismo
root que enviamos. La cadena **lee lo que le dimos**; no cuenta relays por su
cuenta.

**Consecuencias, y son las que más se confunden:**

- **Comparar "relays servidos" contra `num_relays` de la cadena NO valida a la
  cadena: valida a nuestro árbol contra sí mismo.** Si el árbol pierde una hoja,
  la cadena reporta el número perdido sin notar nada.
- **El subreclamo es invisible por diseño.** Reclamar de menos produce un claim
  perfectamente válido.
- La comparación honesta contra la cadena es **hojas del SMST vs `num_relays`**.

### Regla 3 — no todo relay entra al árbol, y eso NO es pérdida

Comentario textual en `claim.go:24-31`: *"not every Relay (Request, Response) pair
in the session is inserted into the tree. The relay hash has to have matched the
difficulty for that service"*, y explica el porqué: *"controlled by the Relay
Mining difficulty to reduce co-processor hardware requirements and enable scaling
to tens of billions of relays"*.

**Consecuencia**: servidos > hojas es lo NORMAL cuando la dificultad no es la
base. Son **tres conteos distintos** —servidos, hojas del árbol, `num_relays`— y
sólo los dos últimos deben coincidir.

Por eso el evento trae `num_relays` **y** `num_estimated_relays`, y
`num_claimed_compute_units` **y** `num_estimated_compute_units`
(`event.pb.go`, `EventClaimUpdated`): lo reclamado es lo que entró al árbol; lo
estimado es lo que ese árbol representa dada la dificultad.

### Regla 4 — dos enumerados distintos, y se confunden

`poktroll/x/proof/types/types.pb.go`:

- **`ClaimProofStage`**: `CLAIMED`, `PROVEN`, `SETTLED`, `EXPIRED` — dónde está el
  claim en su ciclo.
- **`ClaimProofStatus`**: `PENDING_VALIDATION`, `VALIDATED`, `INVALID` — qué se
  concluyó sobre su proof.

El campo que vive EN el claim es `proof_validation_status` (el segundo).

### Regla 5 — los CINCO eventos del módulo proof

`poktroll/x/proof/types/event.pb.go`: `EventClaimCreated`, `EventClaimUpdated`,
`EventProofSubmitted`, `EventProofUpdated`, `EventProofValidityChecked`.

`EventClaimUpdated` lleva: `num_relays`, `num_claimed_compute_units`,
`num_estimated_compute_units`, `num_estimated_relays`, `claimed_upokt`,
`service_id`, `application_address`, `session_end_block_height`,
`supplier_operator_address`, `claim_proof_status_int`.

**`claimed_upokt` está en el evento**: la plata reclamada se puede leer del evento
sin recalcularla.

### Regla 6 — el proof no siempre se exige, y hay TRES parámetros que lo deciden

`poktroll/x/proof/types/params.pb.go`: `ProofRequestProbability`,
`ProofRequirementThreshold`, `ProofMissingPenalty`.

O sea: por encima de un umbral el proof se exige siempre; por debajo se sortea con
una probabilidad. **Que un claim no tenga proof no implica que esté mal.**

### Regla 7 — el que liquida es el módulo tokenomics, y ahí está el slashing

`poktroll/x/tokenomics/keeper/settle_pending_claims.go:296` llama
`slashSupplierStake` para el resultado EXPIRADO, y `:720-728` es la función.

**Consecuencia**: el castigo por un proof faltante o inválido **no** sale del
módulo proof: sale de la liquidación. Un claim expirado es el que cuesta stake.

### Lo que no se puede leer del código

Los valores de mainnet de `ProofRequestProbability`, `ProofRequirementThreshold`
y `ProofMissingPenalty`. Son estado de la cadena.

### Regla 8 — el Proof es EFÍMERO: se valida y se borra en el mismo EndBlocker

`poktroll/x/proof/keeper/validate_proofs.go:46` es `ValidateSubmittedProofs`, que
corre en contexto de EndBlocker (lo dice su propio comentario sobre el gas meter,
`:85-93`). Valida cada proof en una goroutine, espera a todas, cierra el iterador
y entonces:

> *"Delete all the processed proofs from the store since they are no longer
> needed."* (`:104-110`, con `k.RemoveProof(ctx, sessionId, supplierOperatorAddr)`)

**Consecuencia, y es una trampa cara**: consultar el proof para saber si se
incluyó **no funciona** — para cuando se pregunta, ya no existe. La inclusión se
verifica por **`Claim.ProofValidationStatus`**, que sí persiste en el claim.

Un `AllProofs` o un `GetProof` que devuelve vacío significa *"ya se procesó"*, no
*"nunca llegó"*. Los dos casos dan el mismo resultado y **no se pueden distinguir
por ahí**.
