import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react(), {
    name: "relay-preview-api-diagnostic",
    configureServer(server) {
      // Vite's SPA fallback otherwise returns index.html for /api/bootstrap,
      // producing a JSON parser error when a preview is opened without mocks.
      server.middlewares.use("/api", (_request, response) => {
        response.statusCode = 503;
        response.setHeader("Content-Type", "application/json");
        response.setHeader("Cache-Control", "no-store");
        response.end(JSON.stringify({ error: "This is Relay's UI preview server. Use Open in Browser in the Relay desktop menu, or start the full controller with relay ui, to connect to your machines." }));
      });
    },
  }],
  build: { target: "es2022" },
  server: { strictPort: true, port: 5198 },
});
