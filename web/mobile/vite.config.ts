import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const api = (process.env.MORDE_API ?? "127.0.0.1:8080").replace(/^https?:\/\//, "");

export default defineConfig({
  base: "/app/",
  publicDir: false,
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": `http://${api}`,
    },
  },
});
