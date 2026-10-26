interface Section {
  prefix: string;
  populate: (doc: Document, value: unknown) => void;
  collect: (doc: Document) => unknown;
}

export type { Section };
