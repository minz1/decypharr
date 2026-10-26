import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

export default defineConfig({
  base: "./",
  plugins: [tailwindcss()],
  build: {
    manifest: true,
    outDir: "pkg/server/assets/build/vite",
    emptyOutDir: true,
    sourcemap: "hidden",
    rolldownOptions: {
      input: ["pkg/server/ui/styles.css"],
    },
  },
});
