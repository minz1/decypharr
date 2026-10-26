import type { TestProject } from "vitest/node";

declare module "vitest" {
  interface ProvidedContext {
    fixtures: Record<string, string>;
  }
}

type GlobalSetup = (project: TestProject) => void;

export type { GlobalSetup };
