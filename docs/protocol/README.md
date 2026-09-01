## Las reglas del protocolo

Qué hace poktroll, por entidad, y por dónde se mueve la plata. Existe porque
razonar de memoria sobre el protocolo hacía perder la mitad de los casos e
inventar situaciones que no pueden pasar.

**Verificado contra poktroll v0.1.35** — la versión que fija `go.mod:19`.

### La regla de este directorio

**Cada afirmación cita el archivo de poktroll del que sale. Sin cita, no entra.**

Y el corolario, que es el que cuesta: **si está en poktroll, se lee.** El
protocolo es determinista y la fuente está en el module cache; "no verificado"
vale sólo para **estado de la cadena** (los valores de los parámetros en mainnet),
que no se puede leer de la fuente.

### Los documentos

| entidad | lo que más se malinterpreta |
|---|---|
| [supplier](supplier.md) | "activo" es por servicio Y por altura, y no mira el unbonding; un supplier que desmonta **sigue sirviendo y sigue siendo elegido** |
| [session](session.md) | **no hay reparto de claims entre suppliers**: esa lógica está comentada |
| [claim y proof](claim-and-proof.md) | la cantidad se deriva **del root que firmamos nosotros**; el proof es **efímero** |
| [application](application.md) | el piso `B/N` es un **piso, no un techo**, así que cobrar menos de lo reclamado no es pérdida por defecto |

### Cómo se actualiza

Al subir la versión de poktroll: re-leer las citas, no la prosa. Una regla cuya
cita ya no existe **no se reescribe de memoria** — se vuelve a leer.
