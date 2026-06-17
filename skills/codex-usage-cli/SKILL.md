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
  version: "1.0"
---

## When to Use

- Necesitas saber el porcentaje consumido de la cuenta OpenAI activa en OpenCode.
- Necesitas listar el uso general de todas las cuentas OpenAI guardadas.
- Necesitas identificar qué cuenta está activa actualmente en OpenCode.
- Necesitas revisar cuánto falta para el reset/cooldown de cada cuenta.
- Necesitas validar si la rotación automática de cuentas puede ocurrir.

## Critical Patterns

- No leas ni imprimas tokens manualmente: usa el CLI.
- Sin argumentos, el CLI imprime **solo** el `used_percent` de la ventana primaria de la cuenta activa.
- Para reportes humanos, usa `accounts` o `list`.
- La cuenta activa se marca con `*` en la columna `CURRENT` de `accounts`/`list`.
- El store de cuentas usa `user_id` como identidad real, no `accountId`.
- `accountId` es metadata de workspace y no debe usarse para deduplicar cuentas.
- El reset se muestra como tiempo restante, no como timestamp.
- La ventana secundaria (`rate_limit.secondary_window`) es la ventana semanal; se muestra en las columnas `WEEK%` y `WEEK-RESET` de `accounts`/`list`.
- Las filas de `accounts`/`list` se ordenan por `user_id` alfabéticamente. La columna `ID` es estable y se puede pasar a `use #<id>` (requiere el prefijo `#` para evitar colisiones con `user_id` numéricos como `1`/`2`/`10`).
- Para cambiar la cuenta activa en OpenCode, usa `use <selector>` con el índice de `accounts`/`list` como `#<n>`, el `user_id` exacto, o el email (case-insensitive).
- Si el uso de la cuenta activa supera el umbral configurado por la herramienta, el CLI puede rotar a otra cuenta elegible.
- Nunca expongas `openai.access`, bearer tokens ni contenido completo de `auth.json` en respuestas al usuario. `use` no imprime tokens; solo confirma email y `user_id`.

## Commands

```powershell
codex-usage-cli
```

Devuelve solo el porcentaje consumido de la ventana primaria de la cuenta activa, por ejemplo:

```text
37.5
```

```powershell
codex-usage-cli accounts
```

Lista todas las cuentas guardadas con email, porcentaje usado de la ventana primaria y semanal, tiempo restante hasta reset para cada ventana, índice estable y marca de cuenta actual.

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

## Output Interpretation

Ejemplo de `codex-usage-cli accounts`:

```text
ID  CURRENT  EMAIL                USED%  WEEK%  RESET    WEEK-RESET
1   *        current@example.com  22.5   2.5    2h 15m   5d 12h
2            other@example.com    88     12     1d 3h    3d 4h
```

| Columna | Significado |
|---|---|
| `ID` | Índice 1-based estable, útil como argumento de `use <id>` |
| `CURRENT` | `*` indica la cuenta activa en OpenCode |
| `EMAIL` | Email devuelto por el API de uso |
| `USED%` | Porcentaje consumido de la ventana primaria |
| `WEEK%` | Porcentaje consumido de la ventana secundaria (semanal) |
| `RESET` | Tiempo restante hasta reset de la ventana primaria (`1d 3h`, `2h 15m`, `45m`, `now`, `expired`, `-`) |
| `WEEK-RESET` | Tiempo restante hasta reset de la ventana semanal |

## Environment Variables

Usa estas variables solo si necesitas rutas personalizadas:

```powershell
$env:OPENCODE_AUTH_FILE="C:\ruta\auth.json"; codex-usage-cli
```

```powershell
$env:CODEX_USAGE_ACCOUNTS_FILE="C:\ruta\openai-accounts.json"; codex-usage-cli accounts
```

## Agent Workflow

1. Para una respuesta automática o scriptable, ejecuta `codex-usage-cli` y trata stdout como un número.
2. Para una respuesta al usuario sobre varias cuentas, ejecuta `codex-usage-cli accounts`.
3. Resume la tabla sin incluir tokens ni rutas sensibles.
4. Si la cuenta activa aparece con uso alto, menciona el tiempo restante de reset (primario y semanal) y si hay otras cuentas disponibles según la tabla.
5. Si el usuario quiere rotar manualmente a otra cuenta, ejecuta `codex-usage-cli use <selector>` usando `#<ID>`, el `user_id` o el email de la tabla. Confirma el cambio sin imprimir tokens.
6. Si el comando falla por falta de cuentas guardadas, indica que primero debe ejecutarse el CLI con cada cuenta configurada en OpenCode para registrarla.

## Installation Check

```powershell
codex-usage-cli accounts
```

Si el comando no existe, instala globalmente desde el repo con:

```powershell
go install .
```
