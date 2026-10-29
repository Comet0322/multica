Files here are copied into the sandbox user's home directory (`/home/node`).
Put runtime-private agent configuration here, for example:

- `.claude/skills/<skill>/SKILL.md`: Claude Code skills
- `.claude/settings.json`, `.claude.json`: Claude Code settings and MCP servers
- `.codex/config.toml`: Codex settings and MCP servers

Agents use them like any locally installed skill or MCP server. Members do not
see them in Multica because the broker reports runtime-local skills and MCP
servers as unsupported. Do not put secrets here; pass them with
`SANDBOX_ENV_*` broker variables instead.
