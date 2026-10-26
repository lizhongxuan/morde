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
  const realApi = loadEnv(mode, process.cwd(), "VITE_").VITE_MOCK_API !== "1";

  return {
    plugins: [react(), adminHistoryFallback()],
    server: {
      host: "127.0.0.1",
      port: 5173,
      strictPort: true,
      proxy: {
        ...(realApi
          ? {
            "/api": { target: "http://127.0.0.1:7080", changeOrigin: false },
            "/mcp": { target: "http://127.0.0.1:7080", changeOrigin: false },
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
