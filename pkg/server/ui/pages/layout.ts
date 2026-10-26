import { bootPage } from "#ui/lib/common/dom";
import { installBridge } from "#ui/lib/common/legacy-bridge";
import { setupGlobalListeners } from "#ui/lib/common/listeners";
import { setupMobileNavigation } from "#ui/lib/common/nav";
import { setupPasswordToggles } from "#ui/lib/common/password";
import { setupKeyboardShortcuts } from "#ui/lib/common/shortcuts";
import { setupThemeToggle } from "#ui/lib/common/theme";
import { setupVersionInfo } from "#ui/lib/common/version";

installBridge();

for (const [name, setup] of [
  ["theme", setupThemeToggle],
  ["password toggles", setupPasswordToggles],
  ["version", setupVersionInfo],
  ["listeners", setupGlobalListeners],
  ["navigation", setupMobileNavigation],
  ["shortcuts", setupKeyboardShortcuts],
] as const) {
  bootPage(name, () => {
    setup(document);
  });
}
