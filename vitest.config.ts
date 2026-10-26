import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "happy-dom",
    // Fixtures keep their stylesheet links until loadFixture strips them; parsing must not fail on them.
    environmentOptions: { happyDOM: { settings: { handleDisabledFileLoadingAsSuccess: true } } },
    globalSetup: ["vitest.global-setup.ts"],
    include: ["pkg/server/ui/**/*.test.ts"],
    restoreMocks: true,
    unstubGlobals: true,
  },
});
