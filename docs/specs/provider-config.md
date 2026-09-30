# Provider configuration

## Problem Statement

grimalkin is configured with a single environment variable, `AGENT_MODEL`, plus
whatever the Anthropic SDK picks up from the environment. It never sets
thinking or effort, so how hard a model thinks is left to that model's default,
and the defaults differ between backends: Claude 5.x thinks adaptively, some
Ollama models think by default and others (gemma4) don't think at all unless
asked.

The two backends grimalkin talks to both speak the Anthropic Messages wire
format but disagree on how thinking is requested: Claude 5.x rejects
`thinking: enabled` with a 400, while Ollama ignores `thinking: adaptive` on
some models. No single request shape works on both. Pushing that difference
into ad-hoc environment variables means per-backend incantations, silent
reliance on defaults, and alias sprawl.

## Solution

A single config file at `~/.config/grimalkin/config.json` declares named
providers. A provider is a real endpoint: which wire format it speaks, where
it is, how to authenticate, how it expresses thinking, which effort values it
accepts, and which models it offers. Models may override the provider's
thinking and effort settings. The top level holds only the user's choices:
provider, model, effort. All are required; nothing falls back to a default.
The one exception is a model with no effort levels at all: it declares no
`efforts`, grimalkin sends no effort, and the status bar shows the effort as
not applicable.

grimalkin loads and validates the file at startup and refuses to start on any
missing field, unknown key or unlisted choice. The code never contains model
names: it maps the configured `thinking` value onto the request and sends
`effort` through verbatim. The file is the only source: the SDK's own
environment variables are not read.

## User Stories

1. As a user, I want all of grimalkin's configuration in one file under `~/.config/grimalkin/`, so that I know where to look and edit it.
2. As a user, I want to declare several providers in the file, so that I can keep Claude and Ollama set up side by side.
3. As a user, I want to pick the active provider with one top-level field, so that switching backends is a one-word edit.
4. As a user, I want to pick the active model with one top-level field, so that switching models is a one-word edit.
5. As a user, I want to pick the effort level with one top-level field, so that effort means the same thing whichever provider I use.
6. As a user, I want the provider, model and effort to be required, so that my configuration never silently depends on a default.
7. As a user, I want grimalkin to refuse to start when a required field is missing, so that a mistake is caught before any request is made.
8. As a user, I want grimalkin to refuse to start when the chosen provider isn't declared, so that a typo doesn't send requests to the wrong place.
9. As a user, I want grimalkin to refuse to start when the chosen model isn't listed under the chosen provider, so that I find out about a typo at startup rather than from an API error.
10. As a user, I want grimalkin to refuse to start when the chosen effort isn't one the provider (or model) accepts, so that an effort value is never silently ignored or rejected mid-session.
11. As a user, I want each provider to declare its wire format, so that grimalkin knows which adapter speaks to it.
12. As a user, I want grimalkin to refuse to start on an unknown wire format, so that a misconfigured provider fails loudly.
13. As a user, I want each provider to declare its base URL, so that I can point at Anthropic, a local Ollama, or any compatible endpoint.
14. As a user, I want to set how a provider expresses thinking once on the provider, so that I never choose between `adaptive` and `enabled` day to day.
15. As a user, I want to override thinking on an individual model, so that one model with a different quirk (e.g. Claude Haiku 4.5 needing `enabled`) doesn't force a fake second provider.
16. As a user, I want to override the accepted effort values on an individual model, so that models whose effort levels differ from their provider's are validated correctly.
17. As a user, I want to give an API key literally in the file, so that dummy keys like Ollama's are trivial to configure.
18. As a user, I want to give an API key as a command prefixed with `!`, so that real secrets can come from a password manager, keychain or environment without being written into the file.
19. As a user, I want the key command to run only for the selected provider, so that I'm not prompted by password managers for providers I'm not using.
20. As a user, I want the key command to run once at startup, so that I'm not prompted repeatedly during a session.
21. As a user, I want grimalkin to refuse to start when the key command exits non-zero, so that it never sends a request with a key it failed to fetch.
22. As a user, I want grimalkin to refuse to start when the key command prints nothing, so that it never sends a request with an empty key.
23. As a user, I want trailing whitespace trimmed from the key command's output, so that `echo`-style commands work without care.
24. As a user, I want the configured thinking value to map to exactly one thinking setting on the wire, so that what I configured is exactly what the provider receives.
25. As a user, I want the configured effort value sent to the provider verbatim, so that provider-defined effort names (e.g. Ollama's model-defined levels) work.
26. As a user running Ollama, I want thinking turned on for models like gemma4 that don't think by default, so that I get the answer quality thinking provides.
27. As a user running Claude 5.x, I want thinking requested in the form Claude accepts, so that requests don't fail with a 400.
28. As a user, I want tool loops to keep working with thinking on, so that thinking blocks returned by either backend round-trip unchanged.
29. As a user, I want to add a new model to a provider by adding one entry to the file, so that no code change is needed.
30. As a user, I want to add a new compatible endpoint by adding one provider entry, so that no code change is needed.
31. As a user, I want grimalkin to refuse to start on a key it doesn't recognise, so that a typo like `"thinkng"` on a model never silently falls back to the provider's value.
32. As a user, I want every provider and model in the file validated at startup, not just the selected one, so that a broken entry is found when I write it rather than when I switch to it.
33. As a user, I want a key command to be able to prompt me (e.g. a password manager's passphrase prompt), so that interactive secret stores work.
34. As a user, I want credentials in my shell environment (e.g. `ANTHROPIC_AUTH_TOKEN`) never sent anywhere, so that a token meant for one service can't leak to whatever endpoint the config names.
35. As a user, I want to leave `efforts` out for a model that has no effort levels (e.g. gemma4, which only has thinking on/off), so that I don't have to invent a placeholder effort for it.
36. As a user, I want my configured effort ignored, not rejected, when I switch to a model with no effort levels, so that switching models stays a one-field edit.
37. As a user, I want the status bar to show the effort as not applicable for such a model, so that I can see my effort setting isn't being used.

## Implementation Decisions

- **File location and format:** `~/.config/grimalkin/config.json`, JSON, on every platform. The path is literal: not the platform's standard config directory (which on macOS is `~/Library/Application Support`), and `XDG_CONFIG_HOME` is not honoured. For now it is the only configuration source; `AGENT_MODEL` and the built-in default model are removed.
- **Unknown keys are rejected** anywhere in the file, as a startup error naming the key.
- **Shape** (agreed in discussion):

  ```json
  {
    "provider": "ollama",
    "model": "glm-5.3-flash:cloud",
    "effort": "high",
    "providers": {
      "anthropic": {
        "wire": "anthropic-messages",
        "baseUrl": "https://api.anthropic.com",
        "apiKey": "!printenv ANTHROPIC_API_KEY",
        "thinking": "adaptive",
        "efforts": ["low", "medium", "high", "xhigh", "max"],
        "models": {
          "claude-opus-5": {},
          "claude-haiku-4-5-20251001": { "thinking": "enabled" }
        }
      },
      "ollama": {
        "wire": "anthropic-messages",
        "baseUrl": "http://localhost:11434",
        "apiKey": "ollama",
        "thinking": "enabled",
        "models": {
          "glm-5.3-flash:cloud": { "efforts": ["low", "high", "max"] },
          "deepseek-v4.1-flash:cloud": { "efforts": ["low", "high", "max"] },
          "gemma4:31b-cloud": {}
        }
      }
    }
  }
  ```

- **Provider** is a config concept: a named endpoint bundling wire, base URL, key, thinking, efforts and models. It is distinct from the code's current provider, which is really a wire format. The existing Anthropic adapter becomes the implementation of the `anthropic-messages` wire; it is the only wire for now.
- **Models are a map** keyed by model name. An entry may override `thinking` and `efforts`; an empty object inherits both from its provider. Resolution: the model's value if present, else the provider's.
- **`efforts` is optional**, on a provider and on a model. If neither the model nor its provider declares it, the model has no effort levels: grimalkin sends no `output_config.effort`, ignores the top-level `effort`, and the status bar shows the effort as not applicable. This is the one place a configured value is ignored rather than rejected; it keeps switching models a one-field edit. An empty list (`[]`) is an error, not a way of saying "no levels": leaving the field out is the only way. So a provider whose models' levels differ (Ollama) declares no provider-level `efforts`, and each model with levels lists its own; a model under such a provider that lists none has no effort levels (gemma4 in the example).
- **Fields depend on the wire.** Each wire defines which provider and model fields it accepts. `wire`, `baseUrl`, `apiKey`, `models` and `efforts` are common to every wire; `efforts` names are sent verbatim in whatever effort parameter the wire has. `thinking` belongs to the `anthropic-messages` wire only: required there, and an unknown key under any other wire.
- **Thinking** is a closed set: `adaptive`, `enabled`, `disabled`. Each maps to the matching thinking setting on the request; `enabled` also carries a fixed `budget_tokens` that is not configurable (Ollama ignores it; Claude 5.x rejects `enabled` regardless). Any other value is a startup error.
- **Effort** is sent verbatim as `output_config.effort`, so provider-defined names pass through unchanged. When the resolved model has `efforts`, it is validated against that list, never translated between names.
- **API key:** a string. A leading `!` means run the rest with `/bin/sh -c`. The command runs before the TUI starts, with stdin and stderr attached to the terminal so it can prompt; only stdout is captured, with trailing whitespace trimmed. There is no timeout: a waiting prompt is visible and the user can interrupt it. Non-zero exit or empty output is a startup error naming the provider and exit status. Runs once, at startup, for the selected provider only. No escape for literal keys starting with `!`.
- **The config is the only source of endpoint and credentials.** The SDK client is built with its environment defaults disabled, so `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, profiles and federation are never read. Without this, an `ANTHROPIC_AUTH_TOKEN` in the environment is sent as a Bearer header alongside the configured key, to whatever `baseUrl` the config names.
- **Validation at startup**, failing loudly. Structure is checked for every provider and model in the file; only the key command is limited to the selected provider. Errors:
  - top level: missing `provider`, `model`, `effort` or `providers`; `provider` not declared; `model` not listed under it; `effort` not in the resolved `efforts` (when there is one)
  - provider: missing `wire`, `baseUrl`, `apiKey` or `models`; missing `thinking` on an `anthropic-messages` provider; unknown `wire`; `thinking` outside the closed set; empty `efforts`; empty `models`
  - model: override `thinking` outside the closed set; empty override `efforts`
  - anywhere: an unknown key
  - selected provider: key command exits non-zero or prints nothing

  Provider-level `thinking` is required even when every model overrides it: one rule, no exceptions.
- **No model names in code.** Model-specific knowledge lives only in the config file.
- **No file-permission check** on the config file, even when it contains a literal key. Judged too much ceremony.
- **Config loading** lives in a new package under `internal/`, imported only by the composition root and importing nothing from the application. The existing import check already treats it as an adapter, so it needs no change. The composition root builds the adapter from the resolved selection. The adapter's config grows thinking and effort fields and its request builder sets them.

## Testing Decisions

- Tests exercise external behaviour: what the loader accepts, rejects and resolves, and what goes on the wire. Not how the loader is structured.
- **Config loading seam (new):** one public entry point that takes a config path and returns the resolved selection (wire, base URL, key, model, thinking, effort) or an error. Tests write a temporary `config.json` and cover:
  - a valid config resolving
  - each startup error in the validation list, including an unknown key and a bad `thinking` value on both a provider and a model
  - a broken unselected provider failing validation
  - model-level overrides of thinking and efforts
  - a model with no `efforts` on it or its provider resolving to no effort, whatever the top-level `effort` says
  - a literal key
  - a `!` command key: success with trailing whitespace trimmed, non-zero exit, empty output
  - an unselected provider whose key command would fail (`!false`) not being run
- **Wire seam (existing):** extend the Anthropic adapter's `httptest` test, which already records the request body, to assert:
  - `thinking` and `output_config.effort` are sent as configured, and no `output_config` is sent when there is no effort
  - no `Authorization` header is sent when `ANTHROPIC_AUTH_TOKEN` is set in the test's environment
  - a streamed thinking block with an empty signature comes back in the response content and is sent back unchanged on the next request
- **Prior art:** the adapter's existing test streams canned SSE from an `httptest` server and decodes the captured body; the core is tested against port mocks. Tests follow the given/when/then comment convention already used.
- The composition root's wiring is not tested separately.

## Open Questions

1. **Effort levels on Claude models.** Anthropic's docs say effort availability varies by model; the per-model levels for Claude haven't been checked, so the example's provider-level list is unverified for every model.
2. **The fixed `budget_tokens` value** for `enabled` is not chosen. It must be at least 1024 and below max tokens (currently 32,000).
3. **Missing config file.** Presumably a startup error like any other missing field, but not stated.
4. **Whether Ollama's effort levels change behaviour.** glm-5.3-flash's measurements fit its levels (see Further Notes). deepseek-v4.1-flash showed no measurable difference across `low`, `high` and `max`, but only on an easy prompt with two runs each; unverified on harder work.

## Out of Scope

- Environment variables overriding the file. Agreed as eventual behaviour and deferred.
- Showing thinking in the TUI, and requesting `display: "summarized"` from Claude so there is thinking text to show.
- Wire formats other than `anthropic-messages`.
- Auto-detecting the backend from the base URL.
- Making `budget_tokens` or max tokens configurable.
- Checking config file permissions.

## Further Notes

- **Conflict with current code and README:** `AGENT_MODEL`, the built-in default model and the SDK's own `ANTHROPIC_*` environment variables are today's configuration, and the README documents them. All of it is replaced by the file, and the composition root's comment about the SDK reading its own environment variables becomes false. The README's Configuration section will need rewriting.
- **Spike evidence, Ollama 0.30.6** (deepseek-v4.1-flash, glm-5.3-flash, gemma4:31b):
  - Every `thinking` and `effort` variant was accepted without error.
  - `thinking: enabled` was the only value that made all three models think; gemma4 without thinking answered a counting question wrongly.
  - `disabled` worked, but glm leaked `</think>` into its text.
  - `budget_tokens` was ignored.
  - Each model's effort levels are listed by Ollama's `/api/show` endpoint, but only when the request sets `"verbose": true`; without it the `thinking` key is absent, which is why an earlier pass wrongly concluded no model exposed levels. grimalkin does not call this endpoint. It is how to look up the `efforts` to write into the config when adding an Ollama model. Values seen: deepseek-v4.1-flash `false/low/high/max` (default `high`); glm-5.3-flash `low/high/max` (default `max`); gemma4, kimi-k2.6, nemotron-3 `false/true`; minimax-m2.7 `true` only; deepseek-v4-flash and deepseek-v3.2 list nothing.
  - glm-5.3-flash's effort measurements fit those levels: `low` and `high` thought less than no effort, while `medium`, not one of its levels, thought as much as its default `max`. An unlisted name falls back silently, which is what per-model `efforts` validation guards against.
  - A streamed tool loop through the real adapter worked with thinking on: Ollama returns thinking blocks with no signature and accepts them back with an empty signature.
- **Claude evidence is documentation only.** No Anthropic key is available. Anthropic's thinking docs list `enabled` as a 400 on every 5.x model and on Opus 4.7/4.8, and `adaptive` as the default on 5.x.
