---
name: codex-usage-cli
description: >
  Usa codex-usage-cli para consultar el consumo de cuentas OpenAI/ChatGPT guardadas por OpenCode,
  revisar rotación y listar cuentas sin exponer tokens.
  Trigger: Cuando un agente necesite consultar uso de tokens, revisar cuentas OpenAI guardadas,
  verificar cooldown/reset o identificar la cuenta activa en OpenCode.
license: Apache-2.0
metadata:
  author: gonzalez962
  version: "1.2"
---

## When to Use

- Necesitas saber el porcentaje consumido de la cuenta OpenAI activa en OpenCode.
- Necesitas listar el uso general de todas las cuentas OpenAI guardadas.
- Necesitas identificar qué cuenta está activa actualmente en OpenCode.
- Necesitas revisar cuánto falta para el reset/cooldown de cada cuenta.
- Necesitas validar si la rotación automática de cuentas puede ocurrir.
- Necesitas activar/desactivar el toggle de 5 horas o leer su estado actual.

## Critical Patterns

- No leas ni imprimas tokens manualmente: usa el CLI.
- Sin argumentos, el CLI imprime solo el `used_percent` de la ventana relevante de la cuenta activa. Con el toggle de 5 horas activado y la API exponiendo ambas ventanas, imprime la ventana de **5 horas** (`rate_limit.primary_window`); en cualquier otro caso imprime la ventana **semanal**. Con toggle apagado y respuesta dual, el semanal viene de `secondary_window` y se promueve a la primaria canónica antes de imprimir.
- Para reportes humanos, usa `accounts` o `list`.
- `accounts`/`list` rinde una tabla Markdown con `|` como separador de columnas y `-` en la fila separadora debajo del header. La cuenta activa se marca con `*` en la columna `CURRENT`. Con el toggle de 5 horas activado y al menos una cuenta con doble ventana disponible, las columnas son `ID | CURRENT | EMAIL | USED% | WEEK% | RESET | WEEK-RESET`. En cualquier otro caso, las columnas son `ID | CURRENT | EMAIL | WEEK% | WEEK-RESET` (modo semanal único).
- El store de cuentas usa `user_id` como identidad real, no `accountId`.
- `accountId` es metadata de workspace y no debe usarse para deduplicar cuentas.
- El reset se muestra como tiempo restante, no como timestamp.
- El toggle de 5 horas (`config 5h`) está **activado por defecto**. Persiste su estado en un archivo JSON dedicado junto al store de cuentas. `CODEX_USAGE_CONFIG_FILE` permite sobreescribir la ruta.
- Con el toggle **activado** y la API exponiendo ambas ventanas, la ventana 5h vive en `rate_limit.primary_window` y la semanal en `rate_limit.secondary_window`. Si `secondary_window` viene `null` o falta, se trata a `primary_window` como semanal (fallback) y no hay datos de 5h.
- Con el toggle **desactivado**, la CLI ignora por completo la ventana de 5 horas (independiente de lo que el endpoint devuelva). Si la respuesta trae `secondary_window`, su valor semanal se **promueve** a la primaria canónica (UsedPercent/ResetAt) y los campos secundarios se limpian antes de persistir, rotar, calcular cooldown o imprimir. Si `secondary_window` está ausente o es `null`, se conserva `primary_window` como fallback semanal. El endpoint no cambia: la CLI reinterpreta la respuesta según el toggle.
- Las filas de `accounts`/`list` se ordenan por `user_id` alfabéticamente. La columna `ID` es estable y se puede pasar a `use #<id>` (requiere el prefijo `#` para evitar colisiones con `user_id` numéricos como `1`/`2`/`10`).
- Para cambiar la cuenta activa en OpenCode, usa `use <selector>` con el índice de `accounts`/`list` como `#<n>`, el `user_id` exacto, o el email (case-insensitive).
- Adicionalmente al `auth.json` de OpenCode, la CLI sincroniza la cuenta activa al `auth.json` de Pi (clave de primer nivel `openai-codex`) cuando la cuenta activa **cambia**: `use <selector>` y la rotación automática. El comando sin argumentos sin rotación, `accounts`/`list` y la registración de la cuenta actual en el store **no** tocan ninguno de los dos `auth.json`. Por defecto el archivo es `%USERPROFILE%\.pi\agent\auth.json` (o `~/.pi/agent/auth.json` en Unix). La variable `PI_CODING_AGENT_DIR` sobreescribe el directorio y le anexa `auth.json`; acepta el prefijo `~` (semántica Pi).
- Cuando hay cambio, la CLI propaga los campos OAuth de la cuenta seleccionada (`access`, `accountId`, `refresh`, `expires`, `type`) al `auth.json` de OpenCode tanto en la representación plana `openai.*` como en la anidada `openai` (si esta última ya existía), preservando cualquier otro provider y los campos personalizados bajo `openai.*`. Los campos que la cuenta fuente no trae se eliminan de ambas representaciones para que no sobreviva valor obsoleto de la cuenta anterior.
- La credencial sincronizada a Pi se construye **solo** con los campos OAuth de la cuenta seleccionada: `type` (siempre `"oauth"`; la CLI normaliza registros legados que omiten `type`), `access`, `refresh`, `expires` (entero de milisegundos desde epoch, validado sin truncar) y `accountId` (omitido si está ausente o vacío). Metadatos de uso (`usedPercent`, `resetAt`, `secondaryUsedPercent`, `secondaryResetAt`, `cooldownUntil`) y de identidad (`user_id`, `email`) **nunca** se copian a Pi.
- La actualización a Pi reemplaza únicamente la entrada `openai-codex`, preservando las claves y valores de cualquier otro provider (`anthropic`, `google`, etc.) y sus valores anidados. El formateo del archivo (espacios, orden de claves) puede variar porque la CLI lo re-marshala; el archivo se decodifica con `UseNumber` para que números grandes de otros providers (>2⁵³) sobrevivan la lectura/modificación/escritura sin redondeo. El archivo se crea con `0600` y, cuando hay que crearlo, el directorio padre se crea con `0700`; en POSIX un archivo existente se ajusta a `0600` **antes** de escribir. Si la CLI no puede ajustar permisos, aborta sin tocar credenciales. Un archivo vacío se rechaza como malformado (mismo comportamiento que `JSON.parse` de Pi). La CLI adquiere el lock `${auth.json}.lock` como directorio atómico (protocolo `proper-lockfile` de Pi), 10 intentos × 20 ms, y nunca borra locks que no creó.
- Si la cuenta seleccionada no trae los campos OAuth requeridos, la CLI falla con un error claro que **nombra** los campos faltantes pero **nunca** sus valores, y deja ambos `auth.json` intactos. Si OpenCode se actualiza pero la escritura a Pi falla, la CLI devuelve un error de sincronización parcial que nombra ambos stores y tampoco filtra credenciales.
- Umbrales de rotación y cooldown:
  - Toggle **activado**: 5 horas agotada al **80%**, semanal al **98%**. Si ambas ventanas están agotadas, el cooldown persiste hasta el reset más tardío.
  - Toggle **desactivado**: semanal al **98%**, cooldown hasta el reset de esa ventana.
- Elegibilidad de candidatos a rotar (reglas **per-candidato**: el toggle decide el modo de evaluación y la forma persistida del candidato decide qué campos aplican):

  | Toggle | Forma persistida | Umbral primaria | Umbral secundaria | Cooldown |
  |--------|------------------|-----------------|-------------------|----------|
  | activado | dual (con `secondaryUsedPercent` o `secondaryResetAt` persistido) | 80% con `resetAt` futuro | 98% con `secondaryResetAt` futuro | se respeta |
  | activado | semanal pura (sin metadata secundaria) | 98% con `resetAt` futuro | n/a | se respeta |
  | desactivado | dual obsoleto (persiste de corridas previas con 5h activado) | se ignora (también se ignora el `cooldownUntil`, que pudo haberse derivado de la 5h) | 98% con `secondaryResetAt` futuro | se ignora |
  | desactivado | semanal pura | 98% con `resetAt` futuro | n/a | se respeta |

  Si el reset ya pasó o nunca se persistió, el uso alto se considera obsoleto y la cuenta vuelve a ser elegible. Bajo toggle **desactivado** con candidato `dual`, los datos semanales faltantes se tratan como desconocidos y el candidato queda elegible (regla conservadora).
- Nunca expongas `openai.access`, bearer tokens ni contenido completo de los `auth.json` (OpenCode o Pi) en respuestas al usuario. Los errores de validación o de sincronización parcial nombran los campos afectados pero **nunca** sus valores. `use` no imprime tokens; solo confirma email y `user_id`. El comando `config 5h` nunca toca auth ni cuentas guardadas, así que puede ejecutarse incluso antes de registrar cuentas.

## Commands

```powershell
codex-usage-cli
```

Devuelve solo el porcentaje consumido de la ventana relevante de la cuenta
activa (5 horas con toggle activado y doble ventana disponible; semanal en
cualquier otro caso), por ejemplo:

```text
37.5
```

```powershell
codex-usage-cli accounts
```

Lista todas las cuentas guardadas con email, porcentaje(s) usado(s), tiempo
restante hasta reset, índice estable y marca de cuenta actual. El modo de la
tabla (dual con `USED%/WEEK%` o semanal único con `WEEK%`) depende del toggle
y de la disponibilidad de la ventana secundaria.

```powershell
codex-usage-cli list
```

Alias de `accounts`.

```powershell
codex-usage-cli use <selector>
```

Cambia la cuenta activa en OpenCode. Acepta el índice de `accounts`/`list` con
el prefijo `#` (ej. `use #2`), el `user_id` exacto guardado en el store, o el
email (case-insensitive). El prefijo `#` es obligatorio para índices y previene
colisiones con `user_id` numéricos. No expone el access token en `stdout`.

```powershell
codex-usage-cli config 5h on
codex-usage-cli config 5h off
codex-usage-cli config 5h
```

Activa o desactiva el toggle de 5 horas (escribe en el archivo de
configuración dedicado). `config 5h` sin valor imprime el estado efectivo
actual (`on` o `off`). El toggle está **activado por defecto** cuando el
archivo no existe o falta la clave `5h`. No requiere auth ni cuentas
guardadas.

## Output Interpretation

Ejemplo de `codex-usage-cli accounts` con toggle **desactivado** o sin doble
ventana disponible (modo semanal):

```text
| ID | CURRENT | EMAIL               | WEEK% | WEEK-RESET |
| -- | ------- | ------------------- | ----- | ---------- |
| 1  | *       | current@example.com | 22.5  | 5d 12h     |
| 2  |         | other@example.com   | 88    | 1d 3h      |
```

Ejemplo de `codex-usage-cli accounts` con toggle **activado** y al menos una
cuenta con doble ventana (modo dual):

```text
| ID | CURRENT | EMAIL               | USED% | WEEK% | RESET  | WEEK-RESET |
| -- | ------- | ------------------- | ----- | ----- | ------ | ---------- |
| 1  | *       | current@example.com | 35    | 60    | 2h 15m | 1d 3h      |
| 2  |         | other@example.com   | 70    | 40    | 1h 30m | 4d 6h      |
```

| Columna | Significado |
|---|---|
| `ID` | Índice 1-based estable, útil como argumento de `use <id>` |
| `CURRENT` | `*` indica la cuenta activa en OpenCode |
| `EMAIL` | Email devuelto por el API de uso |
| `USED%` | (solo modo dual) Porcentaje consumido de la ventana de 5 horas (`rate_limit.primary_window.used_percent`) |
| `WEEK%` | Porcentaje consumido de la ventana semanal (`rate_limit.secondary_window.used_percent` en modo dual, `rate_limit.primary_window.used_percent` en modo semanal) |
| `RESET` | (solo modo dual) Tiempo restante hasta reset de la ventana de 5 horas (`1d 3h`, `2h 15m`, `45m`, `now`, `expired`, `-`) |
| `WEEK-RESET` | Tiempo restante hasta reset de la ventana semanal (`1d 3h`, `2h 15m`, `45m`, `now`, `expired`, `-`) |

## Environment Variables

Usa estas variables solo si necesitas rutas personalizadas:

```powershell
$env:OPENCODE_AUTH_FILE="C:\ruta\auth.json"; codex-usage-cli
```

```powershell
$env:CODEX_USAGE_ACCOUNTS_FILE="C:\ruta\openai-accounts.json"; codex-usage-cli accounts
```

```powershell
$env:CODEX_USAGE_CONFIG_FILE="C:\ruta\config.json"; codex-usage-cli config 5h
```

```powershell
$env:PI_CODING_AGENT_DIR="C:\ruta\pi"; codex-usage-cli use #2
# También acepta el prefijo ~ para apuntar al home del usuario.
```

`PI_CODING_AGENT_DIR` es un override de **directorio**; la CLI le anexa
`auth.json`. Por defecto se usa `%USERPROFILE%\.pi\agent\auth.json`
(`~/.pi/agent/auth.json` en Unix).

## Agent Workflow

1. Para una respuesta automática o scriptable, ejecuta `codex-usage-cli` y trata stdout como un número (ventana 5h con toggle activado y doble ventana disponible; semanal en cualquier otro caso).
2. Para una respuesta al usuario sobre varias cuentas, ejecuta `codex-usage-cli accounts`.
3. Resume la tabla Markdown sin incluir tokens ni rutas sensibles. Recuerda que las columnas cambian según el toggle (`USED%/WEEK%` vs solo `WEEK%`).
4. Si la cuenta activa aparece con uso alto, menciona el tiempo restante de reset (5h o semanal según corresponda) y si hay otras cuentas disponibles según la tabla.
5. Si el usuario quiere rotar manualmente a otra cuenta, ejecuta `codex-usage-cli use <selector>` usando `#<ID>`, el `user_id` o el email de la tabla. Confirma el cambio sin imprimir tokens. El cambio manual también sincroniza la cuenta seleccionada al `auth.json` de Pi bajo la clave `openai-codex`.
6. Si necesitas consultar o cambiar el toggle de 5 horas, ejecuta `codex-usage-cli config 5h` (lee), `config 5h on` o `config 5h off` (escribe). No requiere auth ni cuentas.
7. Si el comando falla por falta de cuentas guardadas, indica que primero debe ejecutarse el CLI con cada cuenta configurada en OpenCode para registrarla.
8. Si la sincronización a Pi falla (validación o escritura), el error nombra los stores y los campos afectados pero nunca valores de credenciales. Repórtalo al usuario sin incluir rutas sensibles (`~/.pi/agent/auth.json`).

## Installation Check

```powershell
codex-usage-cli accounts
```

Si el comando no existe, instala globalmente desde el repo con:

```powershell
go install .
```