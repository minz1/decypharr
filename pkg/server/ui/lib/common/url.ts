const readURLBase = (doc: Document): string =>
  doc.querySelector<HTMLMetaElement>('meta[name="decypharr-url-base"]')?.content ?? "";

const joinURL = (base: string, path: string): string => {
  const head = base.endsWith("/") ? base : `${base}/`;
  const tail = path.startsWith("/") ? path.slice("/".length) : path;
  return `${head}${tail}`;
};

export { joinURL, readURLBase };
