import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "happy-dom",
    include: ["pkg/server/ui/**/*.test.ts"],
    testTimeout: 5000,
  },
});
