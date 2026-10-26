import { existsSync, readFileSync, readdirSync } from "node:fs";
import path from "node:path";

import type { TestProject } from "vitest/node";

import type { GlobalSetup } from "#ui/lib/testing/provided";

const MIN_FIXTURES = 1;

const fixturesDir = (): string => {
  const { UI_FIXTURES_DIR: fromEnv } = process.env;
  return fromEnv ?? path.resolve("pkg/server/testdata/rendered");
};

const provideFixtures = (provide: TestProject["provide"]): void => {
  const dir = fixturesDir();
  const names = existsSync(dir) ? readdirSync(dir).filter((name) => name.endsWith(".html")) : [];
  if (names.length < MIN_FIXTURES) {
    const missing = new Error("UI fixtures missing: run pnpm fixtures");
    // Vitest 5.0.5 fails to source-map a global setup's stack and prints that failure instead of the message.
    missing.stack = missing.message;
    throw missing;
  }
  provide(
    "fixtures",
    Object.fromEntries(names.map((name) => [path.basename(name, ".html"), readFileSync(path.join(dir, name), "utf8")])),
  );
};

const setup: GlobalSetup = ({ provide }) => {
  provideFixtures(provide);
};

export { setup };
