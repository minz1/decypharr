import { bootPage, byId } from "#ui/lib/common/dom";
import { readURLBase } from "#ui/lib/common/url";
import { bindRegisterPage } from "#ui/lib/register";

bootPage("register", () => {
  bindRegisterPage(
    {
      form: byId("authForm", HTMLFormElement),
      username: byId("username", HTMLInputElement),
      password: byId("password", HTMLInputElement),
      confirmPassword: byId("confirmPassword", HTMLInputElement),
      save: byId("saveAuthBtn", HTMLButtonElement),
      skip: byId("skipAuthBtn", HTMLButtonElement),
    },
    {
      navigate: (url) => {
        globalThis.location.assign(url);
      },
      urlBase: readURLBase(document),
    },
  );
});
