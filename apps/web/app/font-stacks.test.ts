// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const read = (path: string) => readFileSync(resolve(__dirname, path), "utf8");
const webCss = read("./globals.css");
const desktopCss = read("../../desktop/src/renderer/src/globals.css");
const layout = read("./layout.tsx");
const desktopMain = read("../../desktop/src/renderer/src/main.tsx");
const requireFromWeb = createRequire(resolve(__dirname, "../package.json"));

const GENERIC = /^(?:serif|sans-serif|monospace|cursive|fantasy|system-ui|math|emoji|fangsong|ui-[a-z-]+)$/;

function rootVariable(css: string, name: string): string {
  const match = css.match(new RegExp(`^\\s*${name}:\\s*([^;]+);`, "m"));
  if (!match?.[1]) throw new Error(`${name} is not declared`);
  return match[1];
}

/** Families of a stack, with `var(--x)` references resolved against the same stylesheet. */
function families(css: string, stack: string): string[] {
  return stack.split(",").flatMap((part) => {
    const family = part.trim().replace(/^["']|["']$/g, "");
    const variable = family.match(/^var\((--[\w-]+)\)$/)?.[1];
    return variable ? families(css, rootVariable(css, variable)) : [family];
  });
}

function fontsourceImports(source: string): string[] {
  return [...source.matchAll(/^import "(@fontsource-variable\/[^"]+)";$/gm)].map((m) => m[1]!).sort();
}

/** The `font-family` names declared by the stylesheets of the given imports. */
function declaredFamilies(imports: string[]): Set<string> {
  return new Set(
    imports.flatMap((spec) => {
      const css = readFileSync(requireFromWeb.resolve(spec), "utf8");
      return [...css.matchAll(/font-family:\s*['"]([^'"]+)['"]/g)].map((m) => m[1]!);
    }),
  );
}

describe("Appearance Latin faces", () => {
  // The CJK choice follows these in --font-sans and --font-content, so a
  // generic family here would draw Han before the CJK font is reached.
  const names = ["--font-ui-inter", "--font-ui-serif", "--font-ui-mono"];

  it.each(names)("web %s names families only", (name) => {
    expect(families(webCss, rootVariable(webCss, name)).filter((f) => GENERIC.test(f))).toEqual([]);
  });

  it.each(names)("desktop %s names families only", (name) => {
    expect(families(desktopCss, rootVariable(desktopCss, name)).filter((f) => GENERIC.test(f))).toEqual([]);
  });

  it("keeps the onboarding serif ending with its generic fallbacks", () => {
    expect(families(webCss, rootVariable(webCss, "--font-serif")).slice(-2)).toEqual(["Times New Roman", "serif"]);
    expect(families(webCss, rootVariable(webCss, "--font-serif"))).toContain("ui-serif");
  });
});

describe("web font loading", () => {
  // next/font/google downloads from the Google Fonts CDN during `next build`,
  // which fails on networks without internet access.
  it("needs no network at build time", () => {
    expect(layout).not.toMatch(/from "next\/font\/google"/);
  });

  it("loads the same font files as desktop", () => {
    expect(fontsourceImports(layout)).toEqual(fontsourceImports(desktopMain));
  });

  it.each(["--font-inter", "--font-geist-mono", "--font-jetbrains-mono", "--font-source-serif"])(
    "%s names a family the layout loads",
    (name) => {
      const loaded = declaredFamilies(fontsourceImports(layout));
      expect(families(webCss, rootVariable(webCss, name)).filter((f) => !loaded.has(f))).toEqual([]);
    },
  );
});
