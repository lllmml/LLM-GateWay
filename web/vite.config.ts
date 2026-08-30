import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const controlPlaneTarget = "http://127.0.0.1:8081";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
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
