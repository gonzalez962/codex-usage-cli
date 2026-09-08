# codex-usage-cli

## Qué hace la herramienta
CLI en Go para consultar el uso de OpenAI desde OpenCode.

- Sin argumentos: muestra en `stdout` solo el porcentaje usado de la ventana relevante de la cuenta actual. Con el toggle de 5 horas activado y la API exponiendo ambas ventanas, imprime el porcentaje de la ventana de **5 horas** (`rate_limit.primary_window`); en cualquier otro caso (toggle apagado o `secondary_window` ausente/`null`) imprime el porcentaje de la ventana **semanal**.
- Si la ventana activa supera el umbral interno, puede rotar automáticamente a otra cuenta guardada. Con el toggle de 5 horas activado, la cuenta se considera agotada cuando la ventana de 5 horas llega al **80%** o la ventana semanal llega al **98%**. Con el toggle apagado, solo se considera el **98%** sobre la ventana semanal (la CLI promueve `secondary_window` a la primaria canónica cuando está disponible, o conserva `primary_window` como fallback).
- `accounts` y `list`: muestran todas las cuentas guardadas en una tabla Markdown (`|` y `-`). Con el toggle de 5 horas activado y la API exponiendo ambas ventanas para alguna cuenta, la tabla incluye las cuatro columnas de uso (`USED%` 5h + `WEEK%` semanal, `RESET` + `WEEK-RESET`). En cualquier otro caso, la tabla usa el modo semanal único (`WEEK%` / `WEEK-RESET`). La cuenta activa se marca con `*` en la columna `CURRENT`, resuelta desde OpenCode o, si OpenCode no está disponible, desde Pi Agent.
- `use <selector>`: cambia la cuenta activa en OpenCode por una cuenta guardada (sin exponer tokens). El selector es el índice de la tabla con prefijo `#` (`#<n>`), el `user_id` exacto, o el email (case-insensitive).
- `config 5h on|off`: activa o desactiva el toggle de 5 horas. Persiste el estado en un archivo JSON dedicado junto al store de cuentas. `config 5h` (sin valor) imprime el estado efectivo actual (`on`/`off`).
- `config 5h-threshold [percent]` y `config weekly-threshold [percent]`: consultan o actualizan los umbrales de rotación. Sin valor imprime el umbral efectivo como número parseable (default **80** para 5 horas, **98** para semanal). Con valor valida que sea un número finito en `(0, 100]` y lo persiste bajo `5h_threshold` / `weekly_threshold`.

## Requisitos
- Go 1.22 o superior.
- Al menos uno de los dos proveedores de credenciales soportados (ver [Proveedores y fallback](#proveedores-y-fallback)):
  - **OpenCode** (por defecto: `%USERPROFILE%\\.local\\share\\opencode\\auth.json`), o
  - **Pi Agent** (por defecto: `%USERPROFILE%\\.pi\\agent\\auth.json` en Windows, `~/.pi/agent/auth.json` en Unix).

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

## Ventanas de consola en Windows

El binario se enlaza como aplicación de consola, así que ejecutarlo desde una
terminal funciona como cualquier otra CLI. El efecto secundario es que un
proceso padre que lo lance sin `CREATE_NO_WINDOW` (un hook de agente, un
programador de tareas, un wrapper) hace que Windows le asigne una consola nueva
que aparece en pantalla.

Para que la mitigación no dependa de cada llamador, la CLI la resuelve por sí
misma: al arrancar consulta `GetConsoleProcessList`. Si hay exactamente un
proceso adjunto, la consola fue asignada para ella y la oculta con
`ShowWindow(SW_HIDE)`. Si hay dos o más, la consola es la terminal compartida
con tu shell y no se toca, así que el uso interactivo no cambia.

Fuera de Windows la función es un no-op: los sistemas tipo Unix no asignan
ventanas de consola a los procesos hijos, y el comportamiento de la CLI es
idéntico se ejecute desde una terminal o desde un hook de agente.

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
la columna `CURRENT`: se resuelve desde el `auth.json` de OpenCode cuando está
disponible y, si OpenCode no está instalado o su credencial no es usable, desde
la entrada `openai-codex` de Pi Agent (lectura bajo lock, sin escrituras). El
match es por igualdad exacta del access token contra el store, así que no
agrega ninguna consulta al endpoint de uso; si Pi refrescó su credencial por
fuera y el store quedó desactualizado, ninguna fila queda marcada hasta que el
comando sin argumentos vuelva a sincronizar. Las filas se ordenan por `user_id` alfabéticamente y el
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
- Umbrales de rotación y cooldown (configurables; ver `Umbrales de rotación` más abajo):
  - Toggle **on**: 5 horas agotada al **80%**, semanal al **98%**, cooldown toma el reset más tardío si ambas ventanas están agotadas.
  - Toggle **off**: solo semanal al **98%** (usando el valor ya promovido o el fallback de la primaria, según corresponda).
- Persistencia: solo con toggle **on** se guardan `secondaryUsedPercent` y
  `secondaryResetAt`; con toggle **off** se eliminan del store.
- Tabla de `accounts`/`list`: dual (cuatro columnas) cuando el toggle está **on**
  y al menos una cuenta expone la ventana secundaria; semanal única en cualquier
  otro caso.

## Umbrales de rotación (`config 5h-threshold`, `config weekly-threshold`)

Por defecto la CLI considera **agotada** la cuenta activa cuando la ventana de 5
horas llega al **80%** y la ventana semanal al **98%**. Estos umbrales se pueden
ajustar (por ejemplo para rotar antes o después) y se persisten en el mismo
`config.json` que el toggle de 5 horas, bajo las claves `5h_threshold` y
`weekly_threshold`.

```powershell
codex-usage-cli config 5h-threshold 75
```

```powershell
codex-usage-cli config weekly-threshold 99
```

```powershell
codex-usage-cli config 5h-threshold
```

```powershell
codex-usage-cli config weekly-threshold
```

- Sin valor imprime el umbral efectivo como un número parseable (sin
  decoración), de modo que se puede capturar en scripts.
- Con valor, la CLI valida que sea un número finito dentro de `(0, 100]`,
  persiste el cambio y vuelve a imprimir el valor aceptado. Valores fuera de
  rango, no numéricos, `NaN`/`±Inf` o vacíos se rechazan **sin modificar el
  archivo de configuración** ni el toggle de 5 horas.
- El mismo comando funciona aunque no haya auth, cuentas guardadas ni URL de
  uso configurados, igual que `config 5h`.
- Las claves desconocidas y el toggle de 5 horas (`5h`) ya presentes en el
  archivo se conservan intactas. Los permisos del archivo se mantienen en
  `0600` (en Windows `chmod` es no-op pero la API usa `0600` en la creación).
- Los umbrales persistidos se aplican de forma consistente en la rotación de
  la cuenta activa, la elegibilidad de candidatas alternativas y el cálculo de
  `cooldownUntil` (incluyendo la regla "reset más tardío" cuando ambas
  ventanas están agotadas).
- El parser también rechaza valores corruptos leídos desde el archivo: si
  `5h_threshold` o `weekly_threshold` están fuera de `(0, 100]` o no son
  numéricos, la CLI se niega a arrancar antes de hacer cualquier rotación.

## Rotación automática de cuentas

Cuando se ejecuta sin argumentos, la herramienta consulta el endpoint de uso.

Con el toggle de 5 horas **activado**, la API expone dos ventanas:
`rate_limit.primary_window` corresponde a la ventana de **5 horas** y
`rate_limit.secondary_window` corresponde a la ventana **semanal**. La cuenta
activa se considera agotada cuando la ventana de 5 horas llega al **80%** o la
ventana semanal llega al **98%** (estos cortes son los defaults; puedes
ajustarlos con `config 5h-threshold` y `config weekly-threshold`, ver la
sección anterior). Si ambas están agotadas, el cooldown
persiste hasta el reset más tardío.

Con el toggle **desactivado**, la CLI ignora por completo la ventana de 5
horas. Si la API devolvió `secondary_window` con el valor semanal, la CLI lo
**promueve** a la primaria canónica (UsedPercent/ResetAt) y limpia los campos
secundarios antes de persistir, rotar, calcular cooldown o imprimir. Si la
respuesta no incluye `secondary_window`, la CLI conserva `primary_window` como
fallback semanal. En ambos casos la cuenta activa se considera agotada cuando
esa ventana semanal llega al **98%** (default configurable con
`config weekly-threshold`), y el cooldown persiste hasta el reset de esa
misma ventana.

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
campos aplican. Los valores de la tabla son los defaults persistidos; los
umbrales primaria y secundaria se pueden ajustar con `config 5h-threshold` y
`config weekly-threshold` respectivamente. Las reglas son:

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

`accounts`/`list` y la registración ordinaria de la cuenta actual no cambian
las cuentas activas. El comando sin argumentos sí puede actualizar credenciales
sin cambiar de cuenta cuando importa desde Pi una versión OAuth más reciente.

### Reconciliación entrante desde Pi (comando sin argumentos)

Cuando se ejecuta sin argumentos, la CLI también hace una **reconciliación
entrante best-effort** desde el `auth.json` de Pi:

- Lee la entrada `openai-codex` de Pi bajo el mismo protocolo de lock
  `${auth.json}.lock` que usa la sincronización saliente, pero como una
  operación **estrictamente de lectura**: nunca crea ni modifica el archivo
  de auth de Pi durante la inspección. Si el archivo falta, está bloqueado,
  el JSON está malformado, la credencial no parsea o la API rechaza el
  token, el comando continúa normalmente con OpenCode (no propaga error).
- Valida el `access` de Pi contra el endpoint de uso y usa el `user_id`
  devuelto por la API como **única** clave de match contra el store
  normalizado (`accountId` es metadata y nunca se usa para emparejar).
- Si el `access` de Pi es idéntico al de OpenCode, **no** hace una segunda
  llamada a la API: reutiliza el `usage` ya descargado como validación.
- La cuenta guardada debe tener un `expires` entero válido. Para una cuenta no
  activa, Pi debe tener un `expires` estrictamente mayor. Para la cuenta activa,
  tampoco se permite reemplazar una credencial de OpenCode o del store que sea
  más reciente. Si el store ya contiene exactamente la versión de Pi pero
  OpenCode quedó atrás, el siguiente intento actualiza solo OpenCode.
- Mezcla **solo** los campos OAuth de Pi (`type=oauth`, `access`, `refresh`,
  `expires`, `accountId` opcional) sobre la cuenta guardada matcheada;
  preserva `user_id`, `email`, `account` y cualquier metadata personalizada.
  Normaliza y persiste el `usage` de Pi según el toggle de 5 horas actual.
- Cuando la cuenta matcheada es la activa de OpenCode (el `user_id` de la API
  de Pi coincide con el `user_id` de la API de OpenCode): actualiza el tuple
  OAuth completo de OpenCode y devuelve la cuenta reconciliada al resto del
  pipeline; la persistencia normal posterior (`ensureCurrentAccountRegistered`)
  opera sobre la cuenta reconciliada, así que **no puede restaurar
  credenciales viejas**.
- Cuando la cuenta matcheada **no** es la activa: solo importa al store. La
  cuenta activa de OpenCode, el `usage` y el stdout del comando no se tocan
  y nunca se cambia de cuenta solo porque Pi la tenga.

La inspección entrante es best-effort: un archivo de Pi ausente o inválido,
contención del lock, token rechazado, falta de coincidencia o expiraciones no
comparables dejan el comando usando OpenCode sin devolver error. Una vez
confirmada una credencial válida y más reciente, los fallos al persistir el
store o actualizar OpenCode sí se devuelven para hacer visible una
sincronización parcial y permitir reintentarla.

## Proveedores y fallback

Ningún comando exige que ambos proveedores estén instalados. Un proveedor
ausente se omite en lugar de abortar la ejecución:

- `accounts` / `list`: si el `auth.json` de OpenCode no existe o no es
  utilizable, la tabla se imprime igual con todas las cuentas guardadas y
  simplemente ninguna fila lleva el marcador `*` de cuenta activa.
- `use <id>`: solo sincroniza los proveedores presentes. OpenCode cuenta como
  presente cuando su `auth.json` existe (la CLI nunca lo crea); Pi cuenta como
  presente cuando su directorio de agente existe (la CLI puede crear
  `auth.json` dentro, igual que Pi al iniciar sesión, pero nunca fabrica el
  árbol completo). Si ninguno está presente, devuelve el error
  `OpenCode and Pi Agent are not installed or configured`.
- Comando sin argumentos: ver la matriz de disponibilidad más abajo.

El comando sin argumentos soporta dos proveedores independientes de
credenciales OpenAI/ChatGPT:

- **OpenCode**: lee `auth.json` (variable `OPENCODE_AUTH_FILE`).
- **Pi Agent**: lee `auth.json` (variable `PI_CODING_AGENT_DIR`).

Antes de ejecutar el pipeline, la CLI evalúa **disponibilidad** de cada
proveedor y elige un camino según el resultado. Un proveedor se considera
**no disponible** cuando:

- Su `auth.json` no existe, no es legible, o el JSON está malformado.
- La credencial que necesita no es parseable como OAuth válido:
  - **OpenCode**: falta `openai.access` o viene vacío (semántica histórica de
    `extractToken`; no se exige `refresh`, `expires` ni `type`, así que los
    `auth.json` pre-existentes siguen siendo utilizables).
  - **Pi Agent**: falta la entrada `openai-codex` o falla la validación estricta
    `parsePiOpenAICodexEntry` (acceso, refresh, `expires` entero de
    milisegundos, `type=oauth` cuando está presente y `accountId` opcional).

La matriz de comportamiento del comando sin argumentos es:

| OpenCode | Pi      | Comportamiento |
|----------|---------|----------------|
| disponible | disponible | Camino OpenCode preservado exactamente, incluyendo la [reconciliación entrante desde Pi](#reconciliación-entrante-desde-pi-comando-sin-argumentos). |
| disponible | no disponible | Camino OpenCode preservado; la reconciliación entrante se omite como no-op (no se propaga error). |
| no disponible | disponible | **Camino Pi-only**: se consulta el endpoint con el token de Pi, se imprime el `used_percent` en `stdout` y se persiste la cuenta derivada en el store de la CLI. **No** se crea ni se reescribe el `auth.json` de OpenCode y **no** se cambia de cuenta ni se rota. |
| no disponible | no disponible | Error claro que indica que ni OpenCode ni Pi Agent están instalados o configurados. |

El camino Pi-only aplica el toggle de 5 horas, persiste el `usedPercent`,
`resetAt`, los campos `secondary*` bajo 5h ON, y el `cooldownUntil` por la
misma vía que el camino OpenCode, así que el store queda con la misma forma
que produciría una corrida OpenCode para esa misma cuenta. Un Pi fallido al
llamar al endpoint propaga el error sin tocar el store ni el `auth.json` de
OpenCode. Cuando ambos `auth.json` son utilizables pero la consulta inicial
del endpoint con el token de OpenCode falla, la CLI intenta Pi como fallback;
si ambas llamadas a la API fallan, devuelve un diagnóstico de runtime en lugar
del error `OpenCode and Pi Agent are not installed or configured`.

## Sincronización con Pi (`auth.json`)

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