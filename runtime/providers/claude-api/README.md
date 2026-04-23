# providers/claude-api

v1 primary provider. Direct Anthropic API (no CLI dependency).

Implements the provider contract:
- `invoke({context, prompt, systemPrompt, tools, timeout}) → {output, cost, tokens, updated_context}`
- `tokenCount(context) → number`
- `compact(context, hint) → updated_context`

Credentials: read from `<aspect-home>/.credentials/claude-api.json` per `aspect.json.provider_config.credentials_path`.
