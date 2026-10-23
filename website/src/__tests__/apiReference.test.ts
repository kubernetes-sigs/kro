/** @jest-environment node */
import * as fs from "fs";
import * as path from "path";
import * as yaml from "js-yaml";

const DOCS_ROOT = path.resolve(__dirname, "../../docs");
const API_REF = path.join(DOCS_ROOT, "api/api-reference.md");
const CRDS_DIR = path.join(DOCS_ROOT, "api/crds");

describe("api-reference.md", () => {
  let content: string;

  beforeAll(() => {
    content = fs.readFileSync(API_REF, "utf8");
  });

  it("has mdx: format: md front matter", () => {
    expect(content).toMatch(/^---[\s\S]*?mdx:\s*\n\s+format:\s+md[\s\S]*?---/m);
  });

  it("contains a heading for every CRD kind and group", () => {
    const crdFiles = fs.readdirSync(CRDS_DIR).filter((f) => f.endsWith(".yaml"));
    for (const file of crdFiles) {
      const raw = fs.readFileSync(path.join(CRDS_DIR, file), "utf8");
      const crd = yaml.load(raw) as any;
      const kind = crd?.spec?.names?.kind;
      const group = crd?.spec?.group;
      if (kind) {
        expect(content).toContain(`#### ${kind}`);
      }
      if (group) {
        expect(content).toContain(group);
      }
    }
  });

  it("has no bare HTML-like placeholders outside code spans", () => {
    const withoutFrontmatter = content.replace(/^---[\s\S]*?---\n/, "");
    const withoutComments = withoutFrontmatter.replace(/<!--[\s\S]*?-->/g, "");
    const withoutCodeSpans = withoutComments.replace(/`[^`]*`/g, "");
    expect(withoutCodeSpans).not.toMatch(/<(?!br \/>)[A-Za-z][^>]*>/);
  });

  it("has no escaped braces inside code spans", () => {
    const codeSpans = [...content.matchAll(/`([^`]*)`/g)].map((m) => m[1]);
    for (const span of codeSpans) {
      expect(span).not.toContain("\\{");
    }
  });
});
