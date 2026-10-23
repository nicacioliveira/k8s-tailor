# MCP server + embedded React dashboard (the product UI). Serves the built UI at
# "/" and JSON-RPC at "/mcp"; proxies the Go engine (TAILOR_API) server-side.
FROM oven/bun:1

WORKDIR /app

# Install deps against the committed lockfile (CI-parity).
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

# Source + build the dashboard bundle (resources.ts reads it from dist/ at runtime).
COPY . .
RUN bun run build:web

ENV PORT=8020
EXPOSE 8020

# Run the server from source — Bun executes TS natively; dist/ holds the built UI.
CMD ["bun", "run", "server/main.ts"]
