import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Local development contract: the canonical console origin is
// http://127.0.0.1:5173 and its Go Control Plane proxy remains on :8081.
export const controlPlaneTarget = "http://127.0.0.1:8081";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      "/api": {
        target: controlPlaneTarget,
        changeOrigin: true,
      },
      "/auth": {
        target: controlPlaneTarget,
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: "./vitest.setup.ts",
    css: true,
  },
});
