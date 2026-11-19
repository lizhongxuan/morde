import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const portalRoot = fileURLToPath(new URL(".", import.meta.url));

export default defineConfig({
  root: portalRoot,
  base: "/",
  plugins: [react()],
  publicDir: false,
  build: {
    outDir: resolve(portalRoot, "dist"),
    emptyOutDir: false,
    rollupOptions: {
      input: resolve(portalRoot, "admin/index.html"),
      output: {
        entryFileNames: "admin/assets/[name]-[hash].js",
        chunkFileNames: "admin/assets/[name]-[hash].js",
        assetFileNames: "admin/assets/[name]-[hash][extname]",
      },
    },
  },
});
