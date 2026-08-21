import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Build output lands inside the Go module so relayd can embed it.
// `make dashboard` (or `make build`) runs this before compiling the binary.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../relayd/internal/server/webui",
    emptyOutDir: true,
  },
  server: {
    // Dev mode proxies API calls to a locally running relayd server.
    proxy: {
      "/v1": "http://127.0.0.1:7460",
    },
  },
});
