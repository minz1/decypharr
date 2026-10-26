import { describe, expect, it } from "vitest";

import { registrationForm } from "#ui/lib/register";

describe("the registration form data", () => {
  it("carries the username, password and confirmation under the server's field names", () => {
    const form = registrationForm({ username: "admin", password: "secret", confirmPassword: "secret" });
    expect([...form.entries()]).toStrictEqual([
      ["username", "admin"],
      ["password", "secret"],
      ["confirmPassword", "secret"],
    ]);
  });
});
