## Cómo interactúa todo — la vida de un relay, y dónde muerde cada parámetro

Verificado contra **poktroll v0.1.35**. Las fichas por entidad están al lado; esto
es lo que **ninguna de ellas puede mostrar sola**: el orden, y qué depende de qué.

### La cadena del dinero, de punta a punta

```
relay servido
  └─ ¿su hash cumple target_hash?            ← service, dificultad
       no → NO entra al árbol (y NO es pérdida)
       sí → hoja del SMST
              └─ root firmado por NOSOTROS
                   ├─ Count() = num_relays
                   └─ Sum()   = num_claimed_compute_units
                        └─ × multiplicador(target_hash)     ← service
                             = num_estimated_compute_units
                                  └─ × CUTTM / granularidad ← shared
                                       = claimed_upokt
                                            └─ acotado por piso B/N   ← application
                                                 └─ × reparto 0.7      ← tokenomics
                                                      = lo que cobra el supplier
```

**Cada flecha es un lugar donde el número baja legítimamente.** Antes de llamar
"pérdida" a una diferencia hay que saber en cuál de las seis se produjo.

### Los cinco recortes legítimos, en orden

1. **La dificultad** decide qué relay entra al árbol (`service.md`, regla 2). Con
   dificultad base entra todo y el multiplicador es 1.
2. **El multiplicador** vuelve a subir el número para estimar lo real
   (`service.md`, regla 3). Es lo inverso del anterior, no un recorte.
3. **CUTTM / granularidad** convierte unidades de cómputo en uPOKT
   (`claim.go:GetClaimeduPOKT`). Es el precio, y es **global de la red**.
4. **El piso `B/N`** de la application acota lo que se puede cobrar de su
   presupuesto (`application.md`, regla 1). Servir de más se puede pagar del
   sobrante, acotado por `overservicing_bonus_multiplier`.
5. **El reparto de emisión** deja al supplier el **0.7** (`params.md`,
   tokenomics). El 0.3 restante va a DAO, proposer y source owner **por diseño**.

### La agenda, y de qué parámetro depende cada paso

```
sesión ─── sessionEnd
             +ClaimWindowOpenOffset +1  → se puede RECLAMAR
             +ClaimWindowCloseOffset    → se cierra
             +ProofWindowOpenOffset     → se puede PROBAR
             +ProofWindowCloseOffset    → se cierra; después el claim EXPIRA
                                          y expirar CUESTA STAKE
```

Los cuatro offsets son de `x/shared` y **son de governance**: la agenda del miner
no es nuestra, la fija la cadena y puede cambiar sin que toquemos código.

**Y no hay reparto entre suppliers**: la función que lo hacía está comentada
(`session.md`, regla 1), así que todos podemos presentar en la misma altura.

### Quién decide qué, y es donde más se confunde

| la pregunta | la entidad que la responde | la que NO |
|---|---|---|
| ¿este supplier sirve este servicio ahora? | **supplier**, por `ServiceConfigHistory` y altura | no el unbonding |
| ¿entra en la sesión? | **la ventana de config de servicio** | no el unbonding (`supplier.md`, regla 6) |
| ¿cuánto vale un relay? | **service** (CUPR) + `shared` (CUTTM) | no el supplier |
| ¿cuántos relays valen? | **el root que firmamos nosotros** | no la cadena (`claim-and-proof.md`, regla 2) |
| ¿qué gateways puede usar una app? | **la application** (`delegatee_gateway_addresses`) | no el gateway (`gateway.md`) |
| ¿cuánto se puede cobrar? | **la application** (presupuesto y piso) | no el claim |
| ¿cuánto llega al supplier? | **tokenomics** (reparto 0.7) | no el claim |

### Las tres formas que se repiten en las tres entidades stakeables

`supplier`, `application` y `gateway` comparten estructura, y aprenderla una vez
sirve para las tres:

1. Las tres tienen `stake` y `unstake_session_end_height`.
2. Las tres tienen **`UnbondingCanceled`**: el unbonding **es reversible** y nunca
   es un estado terminal.
3. `supplier` y `application` tienen **`StakeStuckInModulePool`**: terminar de
   desmontar **no implica que el owner haya cobrado**.

### Lo que un cambio de governance nos puede hacer sin avisar

- **Mover los cuatro offsets** → cambia cuándo hay que reclamar y probar.
- **Mover `target_num_relays`** → mueve la dificultad → cambia cuántos relays
  entran al árbol, con el mismo tráfico.
- **Mover CUTTM o la granularidad** → cambia el precio de todo lo no liquidado.
- **Mover `mint_allocation_percentages`** → cambia nuestro 0.7.
- **Mover `min_stake` de supplier** → nos puede poner en unbonding **solos**.
- **Mover `num_suppliers_per_session`** → cambia `N`, y `N` es el divisor del piso
  `B/N`: **más suppliers por sesión, piso más chico para cada uno.**

Ninguno de esos requiere que cambiemos código, y todos cambian la plata.

### Lo que NO está leído todavía

Las reglas de una **delegación que cambia a mitad de sesión** y el armado del
anillo de firma (ver `gateway.md`). Es la única pieza de esta serie que sigue sin
citar, y está declarada como tal en vez de completada de memoria.
