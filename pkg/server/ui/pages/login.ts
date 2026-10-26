import { bindLoginForm } from "#ui/lib/login";

const form = document.querySelector<HTMLFormElement>("#loginForm");
if (form !== null) {
  bindLoginForm(form, {
    utils: globalThis.decypharrUtils,
    redirect: (url) => {
      globalThis.location.href = url;
    },
    urlBase: globalThis.urlBase,
  });
}
