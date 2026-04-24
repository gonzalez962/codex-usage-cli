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
- Sin argumentos, el CLI imprime **solo** el `used_percent` de la cuenta activa.
- Para reportes humanos, usa `accounts` o `list`.
- La cuenta activa se marca con `*` en la salida de `accounts`/`list`.
- El store de cuentas usa `user_id` como identidad real, no `accountId`.
- `accountId` es metadata de workspace y no debe usarse para deduplicar cuentas.
- El reset se muestra como tiempo restante, no como timestamp.
- Si el uso de la cuenta activa supera el umbral configurado por la herramienta, el CLI puede rotar a otra cuenta elegible.
- Nunca expongas `openai.access`, bearer tokens ni contenido completo de `auth.json` en respuestas al usuario.

## Commands

```powershell
codex-usage-cli
```

Devuelve solo el porcentaje consumido de la cuenta activa, por ejemplo:

```text
37.5
```

```powershell
codex-usage-cli accounts
```

Lista todas las cuentas guardadas con email, porcentaje usado, tiempo restante hasta reset y marca de cuenta actual.

```powershell
codex-usage-cli list
```

Alias de `accounts`.

## Output Interpretation

Ejemplo de `codex-usage-cli accounts`:

```text
CURRENT  EMAIL                USED%  RESET
*        current@example.com  22.5   2h 15m
         other@example.com    88     45m
```

| Columna | Significado |
|---|---|
| `CURRENT` | `*` indica la cuenta activa en OpenCode |
| `EMAIL` | Email devuelto por el API de uso |
| `USED%` | Porcentaje consumido de la ventana primaria |
| `RESET` | Tiempo restante hasta reset (`1d 3h`, `2h 15m`, `45m`, `now`, `expired`, `-`) |

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
4. Si la cuenta activa aparece con uso alto, menciona el tiempo restante de reset y si hay otras cuentas disponibles según la tabla.
5. Si el comando falla por falta de cuentas guardadas, indica que primero debe ejecutarse el CLI con cada cuenta configurada en OpenCode para registrarla.

## Installation Check

```powershell
codex-usage-cli accounts
```

Si el comando no existe, instala globalmente desde el repo con:

```powershell
go install .
```
