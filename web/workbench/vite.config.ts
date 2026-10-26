import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";
import { loadEnv } from "vite";

export default defineConfig(({ command, mode }) => {
  const env = loadEnv(mode, process.cwd(), ["VITE_", "CH_LOCAL_AIO_"]);
  const realApi = env.VITE_MOCK_API !== "1";
  const localAio = command === "serve" ? env.CH_LOCAL_AIO_URL?.trim() : "";
  const localAioApiKey = command === "serve" ? env.CH_LOCAL_AIO_API_KEY : "";

  if (Boolean(localAio) !== Boolean(localAioApiKey)) {
    throw new Error("Set both CH_LOCAL_AIO_URL and CH_LOCAL_AIO_API_KEY to proxy the local browser");
  }

  const localAioTarget = localAio ? new URL(localAio) : null;
  if (localAioTarget && (
    localAioTarget.protocol !== "http:"
    || !["127.0.0.1", "localhost", "[::1]"].includes(localAioTarget.hostname)
    || localAioTarget.username !== ""
    || localAioTarget.password !== ""
    || localAioTarget.pathname !== "/"
    || localAioTarget.search !== ""
    || localAioTarget.hash !== ""
  )) {
    throw new Error("CH_LOCAL_AIO_URL must be an HTTP loopback origin");
  }
  const browserProxy = {
    target: localAioTarget?.origin ?? "http://127.0.0.1:7081",
    changeOrigin: false,
    ws: true,
    ...(localAioTarget ? { headers: { "X-AIO-API-Key": localAioApiKey } } : {}),
  };

  return {
    base: command === "build" ? "/__ch/wb/" : "/",
    plugins: [react()],
    server: {
      host: "127.0.0.1",
      port: 5174,
      strictPort: true,
      proxy: {
        ...(realApi
          ? {
              "/__ch": { target: "http://127.0.0.1:7081", changeOrigin: false, ws: true },
              "/v1": { target: "http://127.0.0.1:7081", changeOrigin: false, ws: true },
            }
          : {}),
        "/vnc": browserProxy,
        "/websockify": browserProxy,
      },
    },
    test: {
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
      css: true,
    },
  };
});
