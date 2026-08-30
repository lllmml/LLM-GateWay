// @vitest-environment node

import { describe, expect, it } from "vitest";

import viteConfig, { controlPlaneTarget } from "../vite.config";

describe("local development topology", () => {
  it("keeps the canonical console port and control-plane proxy fixed", () => {
    expect(viteConfig.server).toMatchObject({
      port: 5173,
      strictPort: true,
      proxy: {
        "/api": { target: controlPlaneTarget },
        "/auth": { target: controlPlaneTarget },
      },
    });
    expect(controlPlaneTarget).toBe("http://127.0.0.1:8081");
  });
});
