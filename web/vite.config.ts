import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The built bundle is embedded into the Go binary at cmd/server/dist.
export default defineConfig({
  plugins: [react()],
  build: { outDir: "../cmd/server/dist", emptyOutDir: true },
  server: {
    port: 5173,
    proxy: { "/api": { target: "http://localhost:8080", changeOrigin: true } },
  },
});
