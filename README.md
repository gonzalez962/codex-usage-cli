# codex-usage-cli

## Qué hace la herramienta
CLI en Go para consultar el uso de OpenAI desde OpenCode.

- Sin argumentos: muestra en `stdout` solo el porcentaje usado (`used_percent`) de la cuenta actual.
- Si el uso supera el umbral interno (primario >= 80% o semanal >= 98%), puede rotar automáticamente a otra cuenta guardada.
- `accounts` y `list`: muestran todas las cuentas guardadas en una tabla Markdown (`|` y `-`) con cuenta actual, email, `% usado` de la ventana primaria y semanal, y tiempo restante para reset.
- `use <selector>`: cambia la cuenta activa en OpenCode por una cuenta guardada (sin exponer tokens). El selector es el índice de la tabla con prefijo `#` (`#<n>`), el `user_id` exacto, o el email (case-insensitive).

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
Uso actual (solo `used_percent` de la ventana primaria):

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

Salida de ejemplo:

```text
| ID | CURRENT | EMAIL               | USED% | WEEK% | RESET  | WEEK-RESET |
| -- | ------- | ------------------- | ----- | ----- | ------ | ---------- |
| 1  | *       | current@example.com | 22.5  | 2.5   | 2h 15m | 5d 12h     |
| 2  |         | other@example.com   | 88    | 12    | 1d 3h  | 3d 4h      |
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

## Rotación automática de cuentas

Cuando se ejecuta sin argumentos, la herramienta consulta el endpoint de uso y
considera que la cuenta activa está agotada si **cualquiera** de las dos
condiciones se cumple:

- Ventana primaria (`rate_limit.primary_window.used_percent`) >= 80%.
- Ventana semanal (`rate_limit.secondary_window.used_percent`) >= 98% (la
  respuesta trae la ventana secundaria).

En cualquiera de los dos casos, intenta rotar a una cuenta alternativa del store
que:

- No esté en `cooldown` (`cooldownUntil` posterior al momento actual).
- No tenga un `usedPercent` almacenado >= 80% (umbral primario) **y** un
  `resetAt` registrado en el futuro. Si el reset ya pasó o nunca se
  persistió, el uso alto se considera obsoleto y la cuenta vuelve a ser
  elegible.
- No tenga un `secondaryUsedPercent` almacenado >= 98% (umbral semanal) **y**
  un `secondaryResetAt` registrado en el futuro. Misma regla: reset vencido
  o ausente → la cuenta es elegible de nuevo.

Si el store no tiene un `secondaryUsedPercent` guardado para una cuenta
candidata, esa cuenta sigue siendo elegible (decisión conservadora: nunca se
bloquea por datos ausentes, igual que `cooldownUntil` solo bloquea cuando está
presente).

Si no hay candidatos elegibles, la cuenta activa no cambia y `stdout` sigue
imprimiendo solo el `used_percent` de la ventana primaria.

## Variables de entorno
- `OPENCODE_AUTH_FILE`: ruta del `auth.json` de OpenCode.
- `CODEX_USAGE_ACCOUNTS_FILE`: ruta del archivo de cuentas persistidas.

Ejemplos (PowerShell, sesión actual):

```powershell
$env:OPENCODE_AUTH_FILE="C:\\ruta\\auth.json"
```

```powershell
$env:CODEX_USAGE_ACCOUNTS_FILE="C:\\ruta\\openai-accounts.json"
```

## Tests
```powershell
go test ./...
```
