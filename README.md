# codex-usage-cli

## Qué hace la herramienta
CLI en Go para consultar el uso de OpenAI desde OpenCode.

- Sin argumentos: muestra en `stdout` solo el porcentaje usado (`used_percent`) de la cuenta actual.
- Si el uso semanal supera el umbral interno (>= 98%), puede rotar automáticamente a otra cuenta guardada.
- `accounts` y `list`: muestran todas las cuentas guardadas en una tabla Markdown (`|` y `-`) con cuenta actual, email, `% usado` semanal y tiempo restante para reset. La ventana única que expone el endpoint ahora es `rate_limit.primary_window` y representa el uso **semanal**; la vieja `secondary_window` se reporta como `null` y no se usa.
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
Uso actual (solo `used_percent` de la ventana semanal):

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
| ID | CURRENT | EMAIL               | WEEK% | WEEK-RESET |
| -- | ------- | ------------------- | ----- | ---------- |
| 1  | *       | current@example.com | 22.5  | 5d 12h     |
| 2  |         | other@example.com   | 88    | 1d 3h      |
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

Cuando se ejecuta sin argumentos, la herramienta consulta el endpoint de uso.
El endpoint expone una sola ventana (`rate_limit.primary_window`) que
corresponde al uso **semanal**. La herramienta considera que la cuenta activa
está agotada cuando:

- `rate_limit.primary_window.used_percent` >= 98%.

Si se cumple, intenta rotar a una cuenta alternativa del store que:

- No esté en `cooldown` (`cooldownUntil` posterior al momento actual).
- No tenga un `usedPercent` almacenado >= 98% (umbral semanal) **y** un
  `resetAt` registrado en el futuro. Si el reset ya pasó o nunca se
  persistió, el uso alto se considera obsoleto y la cuenta vuelve a ser
  elegible.

El endpoint ya no devuelve `rate_limit.secondary_window` (viene como `null`);
esa ventana (la vieja ventana de 5 horas) ya no se usa ni se persiste, y no
participa en la rotación automática, el cooldown, ni el listado de cuentas.
Las cuentas persistidas por versiones anteriores del CLI se migran de forma
transparente: cualquier `secondaryUsedPercent` / `secondaryResetAt` guardado
se elimina al volver a registrar la cuenta.

Si no hay candidatos elegibles, la cuenta activa no cambia y `stdout` sigue
imprimiendo solo el `used_percent` de la ventana semanal.

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
