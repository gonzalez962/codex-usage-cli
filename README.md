# codex-usage-cli

## Qué hace la herramienta
CLI en Go para consultar el uso de OpenAI desde OpenCode.

- Sin argumentos: muestra en `stdout` solo el porcentaje usado de la ventana relevante de la cuenta actual. Con el toggle de 5 horas activado y la API exponiendo ambas ventanas, imprime el porcentaje de la ventana de **5 horas** (`rate_limit.primary_window`); en cualquier otro caso (toggle apagado o `secondary_window` ausente/`null`) imprime el porcentaje de la ventana **semanal**.
- Si la ventana activa supera el umbral interno, puede rotar automáticamente a otra cuenta guardada. Con el toggle de 5 horas activado, la cuenta se considera agotada cuando la ventana de 5 horas llega al **80%** o la ventana semanal llega al **98%**. Con el toggle apagado, solo se considera el **98%** sobre la ventana semanal (la CLI promueve `secondary_window` a la primaria canónica cuando está disponible, o conserva `primary_window` como fallback).
- `accounts` y `list`: muestran todas las cuentas guardadas en una tabla Markdown (`|` y `-`). Con el toggle de 5 horas activado y la API exponiendo ambas ventanas para alguna cuenta, la tabla incluye las cuatro columnas de uso (`USED%` 5h + `WEEK%` semanal, `RESET` + `WEEK-RESET`). En cualquier otro caso, la tabla usa el modo semanal único (`WEEK%` / `WEEK-RESET`). La cuenta activa se marca con `*` en la columna `CURRENT`.
- `use <selector>`: cambia la cuenta activa en OpenCode por una cuenta guardada (sin exponer tokens). El selector es el índice de la tabla con prefijo `#` (`#<n>`), el `user_id` exacto, o el email (case-insensitive).
- `config 5h on|off`: activa o desactiva el toggle de 5 horas. Persiste el estado en un archivo JSON dedicado junto al store de cuentas. `config 5h` (sin valor) imprime el estado efectivo actual (`on`/`off`).

## Requisitos
- Go 1.22 o superior.
- Archivo de autenticación de OpenCode (por defecto: `%USERPROFILE%\\.local\\share\\opencode\\auth.json`).

## Compilación local
```powershell
go build -o codex-usage-cli.exe .
```

## Instalación global con Go
Desde la raíz del proyecto:

```powershell
go install .
```

## Agregar Go bin al PATH en Windows
```powershell
[Environment]::SetEnvironmentVariable("PATH", [Environment]::GetEnvironmentVariable("PATH", "User") + ";$env:USERPROFILE\go\bin", "User")
```

Luego abre una nueva terminal.

## Uso básico
Porcentaje de la ventana relevante de la cuenta activa (5 horas si el toggle está activado y la API expone ambas ventanas; semanal en cualquier otro caso):

```powershell
codex-usage-cli
```

Listado de cuentas guardadas (dos alias equivalentes):

```powershell
codex-usage-cli accounts
```

```powershell
codex-usage-cli list
```

Salida de ejemplo cuando el toggle de 5 horas está **desactivado** o la API no expone la ventana secundaria (modo semanal):

```text
| ID | CURRENT | EMAIL               | WEEK% | WEEK-RESET |
| -- | ------- | ------------------- | ----- | ---------- |
| 1  | *       | current@example.com | 22.5  | 5d 12h     |
| 2  |         | other@example.com   | 88    | 1d 3h      |
```

Salida de ejemplo cuando el toggle de 5 horas está **activado** y la API expone ambas ventanas para alguna cuenta (modo dual):

```text
| ID | CURRENT | EMAIL               | USED% | WEEK% | RESET  | WEEK-RESET |
| -- | ------- | ------------------- | ----- | ----- | ------ | ---------- |
| 1  | *       | current@example.com | 35    | 60    | 2h 15m | 1d 3h      |
| 2  |         | other@example.com   | 70    | 40    | 1h 30m | 4d 6h      |
```

La tabla usa formato Markdown con `|` como separador de columnas y `-` en la
fila separadora justo debajo del header. La cuenta activa se marca con `*` en
la columna `CURRENT`. Las filas se ordenan por `user_id` alfabéticamente y el
`ID` de la izquierda es estable: lo pasás a `use #<n>` (con el prefijo `#`
obligatorio) para cambiar la cuenta activa por esa fila.

## Cambiar la cuenta activa

```powershell
codex-usage-cli use <selector>
```

Donde `<selector>` puede ser:

- `#<n>`: el índice (1-based) que aparece en la columna `ID` de `accounts`/`list`
  (ej. `use #2`). El prefijo `#` es **obligatorio** para usar el índice y evita
  colisiones con `user_id` numéricos (por ejemplo `use 2` con `user_id` "2"
  cuando también existe "10"). Sin el `#`, el argumento se trata como `user_id`
  o email, no como índice.
- El `user_id` exacto guardado en el store.
- El email asociado a la cuenta (case-insensitive).

Ejemplos:

```powershell
codex-usage-cli use #2
```

```powershell
codex-usage-cli use user-other
```

```powershell
codex-usage-cli use other@example.com
```

El comando escribe `openai.access` (y `openai.accountId` si está disponible) en el
`auth.json` de OpenCode. La salida en `stdout` confirma el email y `user_id` de
la cuenta activa, pero **nunca** incluye el access token.

## Toggle de 5 horas (`config 5h`)

El toggle de 5 horas controla si la CLI consulta y muestra la ventana de 5 horas que algunas respuestas del endpoint incluyen como `rate_limit.primary_window` (la semanal, en ese mismo contrato, vive en `rate_limit.secondary_window`). Por defecto el toggle está **activado**.

```powershell
codex-usage-cli config 5h on
```

```powershell
codex-usage-cli config 5h off
```

```powershell
codex-usage-cli config 5h
```

`config 5h on|off` activa o desactiva el toggle y lo persiste en un archivo JSON
dedicado (`config.json`, junto al store de cuentas). `config 5h` (sin valor)
imprime el estado efectivo (`on` o `off`). El comando no requiere auth, cuentas
guardadas ni URL de uso, así que puede ejecutarse incluso antes de haber
registrado ninguna cuenta o configurar la CLI.

El toggle determina cómo se mapean las dos ventanas que el endpoint puede
devolver (`primary_window` = 5 horas, `secondary_window` = semanal) a las
columnas, persistencia, rotación, cooldown y stdout de la CLI:

- Sin argumentos y con dual disponible: imprime `USED%` de 5 horas (la primaria canónica).
- Sin argumentos y sin dual disponible (o toggle apagado): imprime `WEEK%`.
  Cuando el toggle está apagado y la respuesta trae ambas ventanas, el valor
  semanal viene de `secondary_window` y se **promueve** a la primaria canónica
  antes de cualquier persistencia, rotación, cooldown o stdout. Si la
  respuesta no trae `secondary_window`, el valor semanal se conserva desde
  `primary_window` como fallback.
- Umbrales de rotación y cooldown:
  - Toggle **on**: 5 horas agotada al **80%**, semanal al **98%**, cooldown toma el reset más tardío si ambas ventanas están agotadas.
  - Toggle **off**: solo semanal al **98%** (usando el valor ya promovido o el fallback de la primaria, según corresponda).
- Persistencia: solo con toggle **on** se guardan `secondaryUsedPercent` y
  `secondaryResetAt`; con toggle **off** se eliminan del store.
- Tabla de `accounts`/`list`: dual (cuatro columnas) cuando el toggle está **on**
  y al menos una cuenta expone la ventana secundaria; semanal única en cualquier
  otro caso.

## Rotación automática de cuentas

Cuando se ejecuta sin argumentos, la herramienta consulta el endpoint de uso.

Con el toggle de 5 horas **activado**, la API expone dos ventanas:
`rate_limit.primary_window` corresponde a la ventana de **5 horas** y
`rate_limit.secondary_window` corresponde a la ventana **semanal**. La cuenta
activa se considera agotada cuando la ventana de 5 horas llega al **80%** o la
ventana semanal llega al **98%**. Si ambas están agotadas, el cooldown
persiste hasta el reset más tardío.

Con el toggle **desactivado**, la CLI ignora por completo la ventana de 5
horas. Si la API devolvió `secondary_window` con el valor semanal, la CLI lo
**promueve** a la primaria canónica (UsedPercent/ResetAt) y limpia los campos
secundarios antes de persistir, rotar, calcular cooldown o imprimir. Si la
respuesta no incluye `secondary_window`, la CLI conserva `primary_window` como
fallback semanal. En ambos casos la cuenta activa se considera agotada cuando
esa ventana semanal llega al **98%**, y el cooldown persiste hasta el reset de
esa misma ventana.

En ambos modos, si se cumple el umbral se intenta rotar a una cuenta
alternativa del store que:

- Cumpla los requisitos de `cooldown` que correspondan según la matriz de la
  siguiente sección: en `off` con un candidato `dual` obsoleto se **ignora**
  el `cooldownUntil` que pudo haberse derivado bajo modo dual; en
  `semanal pura` (off) o en cualquier candidato con toggle `on` el
  `cooldownUntil` posterior al momento actual se respeta y bloquea al
  candidato.
- No tenga un `usedPercent` almacenado por encima del umbral aplicable **y** un
  `resetAt` registrado en el futuro. Si el reset ya pasó o nunca se
  persistió, el uso alto se considera obsoleto y la cuenta vuelve a ser
  elegible.

La elegibilidad es **per-candidato**: el toggle decide el modo de evaluación
y la forma persistida del candidato (dual vs. semanal pura) decide qué
campos aplican. Las reglas son:

| Toggle | Forma persistida | Umbral primaria | Umbral secundaria | Cooldown |
|--------|------------------|-----------------|-------------------|----------|
| on     | dual             | 80%             | 98%               | se respeta |
| on     | semanal pura     | 98%             | n/a               | se respeta |
| off    | dual obsoleto    | se ignora (también se ignora el `cooldownUntil`, que pudo haberse derivado de la 5h) | 98%               | se ignora |
| off    | semanal pura     | 98%             | n/a               | se respeta |

Un candidato se detecta como `dual` si tiene `secondaryUsedPercent` o
`secondaryResetAt` persistido, lo que cubre también entradas legadas
escritas antes de que existiera la metadata completa. Bajo toggle **off**
con un candidato `dual`, los datos semanales faltantes se tratan como
"desconocidos" y el candidato queda elegible (regla conservadora: nunca
se bloquea por ausencia de datos).

Si no hay candidatos elegibles, la cuenta activa no cambia y `stdout` sigue
imprimiendo solo el `used_percent` de la ventana relevante.

## Sincronización con Pi (`auth.json`)

Además de actualizar el `auth.json` de OpenCode, la CLI sincroniza la cuenta
activa al `auth.json` de Pi (por defecto `%USERPROFILE%\\.pi\\agent\\auth.json`,
o `~/.pi/agent/auth.json` en Unix). La sincronización ocurre **únicamente**
cuando la cuenta activa cambia:

- `use <selector>` (cambio manual): sincroniza la cuenta seleccionada a Pi.
- Rotación automática sin argumentos: sincroniza la cuenta alternativa elegida.

El comando sin argumentos cuando la cuenta activa **no** cambia, `accounts`/`list`
y la registración de la cuenta actual en el store **no** tocan ninguno de los
dos archivos de auth.

Cuando el cambio ocurre, la CLI actualiza ambos archivos de forma coherente:
los campos OAuth de la cuenta seleccionada (`access`, `accountId`, `refresh`,
`expires`, `type`) se propagan al `auth.json` de OpenCode en la
representación plana `openai.*` (siempre) y en la anidada `openai` (solo
si esa representación ya existía), y simultáneamente se construye la
credencial de Pi que se describe abajo. Antes de tocar **ninguno** de los
dos archivos, la CLI valida que la cuenta seleccionada traiga los campos
OAuth requeridos; si falta alguno, ambos archivos quedan intactos y se
devuelve un error que nombra los campos pero nunca sus valores.

La entrada sincronizada vive bajo la clave de primer nivel `openai-codex` y se
construye únicamente con los campos OAuth de la cuenta seleccionada:

```json
{
  "openai-codex": {
    "type": "oauth",
    "access": "...",
    "refresh": "...",
    "expires": 1777014899000,
    "accountId": "..."
  }
}
```

- `expires` se expresa en milisegundos desde epoch.
- `type` **siempre** se serializa como `"oauth"`. Si la cuenta fuente omite
  `type` (registro legado anterior a que Pi lo exigiera) la CLI lo normaliza
  a `"oauth"` antes de escribir; una cuenta que declare un `type` distinto
  de `"oauth"` rechaza el cambio con un error que nombra el campo pero
  **nunca** sus valores.
- `accountId` se incluye solo si está presente y no está vacío.
- Metadatos de uso (`usedPercent`, `resetAt`, `secondaryUsedPercent`,
  `secondaryResetAt`, `cooldownUntil`) y de identidad (`user_id`, `email`)
  nunca se copian a la credencial de Pi.

La actualización reemplaza **únicamente** la entrada `openai-codex` preservando
las claves y valores de cualquier otro provider (por ejemplo `anthropic`,
`google`, etc.) y sus valores anidados. El formateo del archivo
(espacios, orden de claves) puede variar porque la CLI lo re-marshala, pero
las claves, los valores escalares y la estructura se conservan exactamente.
Para números grandes más allá del rango entero exacto de `float64`
(> 2⁵³) el archivo se decodifica con `UseNumber`, así que la proyección
nunca trunca ni redondea identificadores enteros de otros providers.

El archivo se crea con permisos `0600` y, cuando la CLI tiene que crearlo
desde cero, el directorio padre se crea con `0700`. Un archivo POSIX
existente se ajusta a `0600` **antes** de escribirlo (si el `chmod` falla
la escritura se aborta sin tocar el contenido). Si el JSON está malformado,
si el archivo está vacío (rechazado igual que `JSON.parse` de Pi) o si el
contenido de primer nivel no es un objeto, la CLI aborta sin sobrescribir.

La CLI adquiere el lock `${auth.json}.lock` como un directorio atómico
(protocolo `proper-lockfile` de Pi), reintentando hasta 10 veces con
esperas de 20 ms; nunca borra un lock que no creó. El lock cubre la
secuencia completa de lectura/modificación/escritura y se libera al
retornar, incluso en error.

### Cuándo falla la sincronización

La sincronización valida la cuenta seleccionada **antes** de modificar
cualquier archivo de auth. Si la cuenta no trae los campos OAuth requeridos
(`access`, `refresh`, `expires` como entero de milisegundos, y `type` igual a
`"oauth"` cuando está presente), la CLI devuelve un error claro que nombra
los campos faltantes pero **nunca** sus valores, y deja ambos archivos de auth
intactos.

Si la actualización de OpenCode tiene éxito pero la escritura a Pi falla, la
CLI devuelve un error explícito de sincronización parcial que nombra ambos
stores y tampoco filtra credenciales.

## Variables de entorno
- `OPENCODE_AUTH_FILE`: ruta del `auth.json` de OpenCode.
- `PI_CODING_AGENT_DIR`: ruta del directorio donde Pi guarda su `auth.json`. Si
  está definida y no está vacía, la CLI la usa como override de directorio y
  le anexa `auth.json`. Se admite el prefijo `~` (Pi semantics), que se
  expande al directorio home del usuario. Por defecto se usa
  `%USERPROFILE%\\.pi\\agent\\auth.json` (o `~/.pi/agent/auth.json` en Unix).
- `CODEX_USAGE_ACCOUNTS_FILE`: ruta del archivo de cuentas persistidas.
- `CODEX_USAGE_CONFIG_FILE`: ruta del archivo de configuración del toggle de 5 horas.

Ejemplos (PowerShell, sesión actual):

```powershell
$env:OPENCODE_AUTH_FILE="C:\\ruta\\auth.json"
```

```powershell
$env:PI_CODING_AGENT_DIR="C:\\ruta\\pi" # la CLI usará C:\\ruta\\pi\\auth.json
```

```powershell
$env:PI_CODING_AGENT_DIR="~/mi-pi" # la CLI expande ~ al home del usuario
```

```powershell
$env:CODEX_USAGE_ACCOUNTS_FILE="C:\\ruta\\openai-accounts.json"
```

```powershell
$env:CODEX_USAGE_CONFIG_FILE="C:\\ruta\\config.json"
```

## Seguridad

La CLI nunca imprime `openai.access`, refresh tokens ni el contenido completo
de los `auth.json` (OpenCode o Pi) en respuestas, logs o mensajes de error.
Los errores de validación o sincronización parcial nombran los campos
afectados pero **nunca** sus valores. La salida de `use` confirma el email y
`user_id` de la cuenta activa y omite el access token. Los archivos de auth
se crean y mantienen con permisos `0600` en POSIX para evitar filtraciones
laterales.

## Tests
```powershell
go test ./...
```