import { readdirSync } from "node:fs";

import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

const PAGES = "pkg/server/ui/pages";

const pages = readdirSync(PAGES)
  .filter((name) => name.endsWith(".ts") && !name.endsWith(".test.ts"))
  .map((name) => `${PAGES}/${name}`);
pages.sort((left, right) => left.localeCompare(right));

export default defineConfig({
  base: "./",
  plugins: [tailwindcss()],
  build: {
    manifest: true,
    outDir: "pkg/server/assets/build/vite",
    emptyOutDir: true,
    sourcemap: "hidden",
    rolldownOptions: {
      input: ["pkg/server/ui/styles.css", ...pages],
    },
  },
});
