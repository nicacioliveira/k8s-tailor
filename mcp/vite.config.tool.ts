import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { type Plugin, defineConfig } from "vite";
import { viteSingleFile } from "vite-plugin-singlefile";

// Builds one MCP-app UI entry into a single self-contained HTML file
// (all JS/CSS/fonts inlined) at dist/client/web/entries/<entry>.html — the body
// that resources.ts serves as the ui:// resource and main.ts serves at /.
// Styling: Tailwind v4 over the project's design tokens (web/ds/).
const entry = process.env.TOOL_ENTRY ?? "dashboard-ui";

/**
 * @modelcontextprotocol/ext-apps lazily does `await import("zod/v4")` only to call
 * `z.toJSONSchema` when an app registers its own tools (we don't). A dynamic import of
 * the whole namespace defeats tree-shaking, and the single-file build inlines dynamic
 * chunks, so it shipped every zod locale (~170 kB minified) for a code path we never
 * hit. Point that one import at a module exposing just `z.toJSONSchema` — same
 * behaviour if it ever runs, without the locales.
 */
function slimZodDynamicImport(): Plugin {
  const VIRTUAL = "\0zod-json-schema-only";
  return {
    name: "slim-zod-dynamic-import",
    enforce: "pre",
    transform(code, id) {
      if (!id.includes("@modelcontextprotocol/ext-apps") || !code.includes('import("zod/v4")'))
        return null;
      return {
        code: code.replaceAll('import("zod/v4")', 'import("virtual:zod-json-schema-only")'),
        map: null,
      };
    },
    resolveId(id) {
      return id === "virtual:zod-json-schema-only" ? VIRTUAL : null;
    },
    load(id) {
      if (id !== VIRTUAL) return null;
      return 'import { toJSONSchema } from "zod/v4";\nexport const z = { toJSONSchema };\n';
    },
  };
}

export default defineConfig({
  plugins: [slimZodDynamicImport(), react(), tailwindcss(), viteSingleFile()],
  build: {
    outDir: "dist/client",
    emptyOutDir: false,
    rollupOptions: { input: `web/entries/${entry}.html` },
  },
});
