import type { TestProject } from "vitest/node";

declare module "vitest" {
  interface ProvidedContext {
    fixtures: Record<string, string>;
    iconClasses: string[];
    iconCSS: string;
    configSample: unknown;
    configExclusions: unknown;
    configResponses: Record<string, unknown>;
  }
}

type GlobalSetup = (project: TestProject) => void;

export type { GlobalSetup };
