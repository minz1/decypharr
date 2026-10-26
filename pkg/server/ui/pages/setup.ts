import { bootPage } from "#ui/lib/common/dom";
import { readURLBase } from "#ui/lib/common/url";
import { startSetupWizard } from "#ui/lib/setup/wizard";

bootPage("setup", async () =>
  startSetupWizard({
    navigate: (url) => {
      globalThis.location.assign(url);
    },
    urlBase: readURLBase(document),
  }),
);
