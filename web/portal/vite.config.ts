import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vitest/config";
import { loadEnv } from "vite";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const portalRoot = dirname(fileURLToPath(import.meta.url));

function adminHistoryFallback(): Plugin {
  return {
    name: "morde-admin-history-fallback",
    configureServer(server) {
      server.middlewares.use((request, _response, next) => {
        const pathname = request.url?.split(/[?#]/, 1)[0];
        if ((request.method === "GET" || request.method === "HEAD") && pathname?.startsWith("/admin/")
          && pathname !== "/admin/index.html" && !pathname.includes(".")) {
          request.url = "/admin/index.html";
        }
        next();
      });
    },
  };
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "VITE_");
  const realApi = env.VITE_MOCK_API !== "1";
  const apiTarget = env.VITE_PORTAL_API_PROXY_TARGET || "http://127.0.0.1:7080";

  return {
    plugins: [react(), adminHistoryFallback()],
    server: {
      host: "127.0.0.1",
      port: 5173,
      strictPort: true,
      ...(process.env.CH_E2E_USE_POLLING === "1"
        ? { watch: { usePolling: true, interval: 1_000 } }
        : {}),
      proxy: {
        ...(realApi
          ? {
            "/api": { target: apiTarget, changeOrigin: false },
            "/mcp": { target: apiTarget, changeOrigin: false },
            }
          : {}),
      },
    },
    build: {
      rollupOptions: {
        input: {
          portal: resolve(portalRoot, "index.html"),
          admin: resolve(portalRoot, "admin/index.html"),
        },
      },
    },
    test: {
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
      css: true,
    },
  };
});
