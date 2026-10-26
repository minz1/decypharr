import { existsSync, readFileSync, readdirSync } from "node:fs";
import path from "node:path";

import type { TestProject } from "vitest/node";

import type { GlobalSetup } from "#ui/lib/testing/provided";

const fixturesDir = (): string => {
  const { UI_FIXTURES_DIR: fromEnv } = process.env;
  return fromEnv ?? path.resolve("pkg/server/testdata/rendered");
};

const provideFixtures = (provide: TestProject["provide"]): void => {
  const dir = fixturesDir();
  const names = existsSync(dir) ? readdirSync(dir).filter((name) => name.endsWith(".html")) : [];
  if (names.length === 0) {
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

const ICON_SOURCES = [
  { dir: "pkg/server/templates", ext: ".html" },
  { dir: "pkg/server/ui", ext: ".ts" },
  { dir: "pkg/server/assets/js", ext: ".js" },
];

const sourceFiles = (dir: string, ext: string): string[] =>
  existsSync(dir)
    ? readdirSync(dir, { recursive: true, encoding: "utf8" })
        .filter((name) => name.endsWith(ext) && !name.endsWith(".test.ts"))
        .map((name) => path.join(dir, name))
    : [];

const provideIcons = (provide: TestProject["provide"]): void => {
  const used = new Set<string>();
  for (const { dir, ext } of ICON_SOURCES) {
    for (const file of sourceFiles(dir, ext)) {
      for (const [iconClass] of readFileSync(file, "utf8").matchAll(/\bbi-[a-z0-9-]+/gu)) {
        used.add(iconClass);
      }
    }
  }
  provide("iconClasses", [...used]);
  provide("iconCSS", readFileSync("node_modules/bootstrap-icons/font/bootstrap-icons.css", "utf8"));
};

const setup: GlobalSetup = ({ provide }) => {
  provideFixtures(provide);
  provideIcons(provide);
};

export { setup };
