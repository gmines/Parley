import { defineConfig } from "vite";

// Tauri expects a fixed port and fails if it is taken.
export default defineConfig({
  clearScreen: false,
  server: { port: 1420, strictPort: true },
  envPrefix: ["VITE_", "TAURI_ENV_"],
  build: {
    target: "es2022",
    outDir: "dist",
    sourcemap: !!process.env.TAURI_ENV_DEBUG,
  },
});
