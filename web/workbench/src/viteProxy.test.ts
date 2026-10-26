import { afterEach, describe, expect, it, vi } from "vitest";
import viteConfig from "../vite.config";

const devEnv = {
  command: "serve",
  mode: "development",
  isSsrBuild: false,
  isPreview: false,
} as const;

afterEach(() => vi.unstubAllEnvs());

describe("Vite API proxy", () => {
  it("proxies API routes by default", async () => {
    vi.stubEnv("VITE_MOCK_API", "");
    const config = await viteConfig(devEnv);

    expect(config.server?.proxy).toHaveProperty("/__ch");
    expect(config.server?.proxy).toHaveProperty("/v1");
  });

  it("keeps API routes local when mock mode is explicitly enabled", async () => {
    vi.stubEnv("VITE_MOCK_API", "1");
    const config = await viteConfig(devEnv);

    expect(config.server?.proxy).not.toHaveProperty("/__ch");
    expect(config.server?.proxy).not.toHaveProperty("/v1");
    expect(config.server?.proxy).toHaveProperty("/vnc");
  });
});
