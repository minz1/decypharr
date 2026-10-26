import { joinURL, readURLBase } from "#ui/lib/common/url";

const fetcher = async (endpoint: string, init: RequestInit = {}): Promise<Response> => {
  const headers = new Headers(init.body instanceof FormData ? {} : { "Content-Type": "application/json" });
  for (const [name, value] of new Headers(init.headers)) {
    headers.set(name, value);
  }
  return fetch(joinURL(readURLBase(document), endpoint), { ...init, headers });
};

export { fetcher };
