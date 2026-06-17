# codex-usage-cli

## Qué hace la herramienta
CLI en Go para consultar el uso de OpenAI desde OpenCode.

- Sin argumentos: muestra en `stdout` solo el porcentaje usado (`used_percent`) de la cuenta actual.
- Si el uso supera el umbral interno, puede rotar automáticamente a otra cuenta guardada.
- `accounts` y `list`: muestran todas las cuentas guardadas con cuenta actual, email, `% usado` de la ventana primaria y semanal, y tiempo restante para reset.
- `use <id>`: cambia la cuenta activa en OpenCode por una cuenta guardada (sin exponer tokens).

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
ID  CURRENT  EMAIL                USED%  WEEK%  RESET    WEEK-RESET
1   *        current@example.com  22.5   2.5    2h 15m   5d 12h
2            other@example.com    88     12     1d 3h    3d 4h
```

La cuenta activa se marca con `*` en la columna `CURRENT`. Las filas se ordenan por
`user_id` alfabéticamente y el `ID` de la izquierda es estable: lo podés usar con
`use <id>` para cambiar la cuenta activa.

## Cambiar la cuenta activa

```powershell
codex-usage-cli use <id>
```

Donde `<id>` puede ser:

- `#<n>`: el índice (1-based) que aparece en la columna `ID` de `accounts`/`list`
  (ej. `use #2`). El prefijo `#` es obligatorio para usar el índice y evita
  colisiones con `user_id` numéricos (por ejemplo `use 2` con `user_id` "2"
  cuando también existe "10").
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
