import { setButtonLoading } from "#ui/lib/common/button";
import { fetcher } from "#ui/lib/common/http";
import { createToast } from "#ui/lib/common/toast";
import { readURLBase } from "#ui/lib/common/url";
import { bindLoginForm } from "#ui/lib/login";

const form = document.querySelector<HTMLFormElement>("#loginForm");
if (form !== null) {
  bindLoginForm(form, {
    utils: { fetcher, createToast, setButtonLoading },
    redirect: (url) => {
      globalThis.location.href = url;
    },
    urlBase: readURLBase(document),
  });
}
