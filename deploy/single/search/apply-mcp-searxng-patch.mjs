import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

export const PATCH_MARKER = "MORDE_PAGE_DATE_METADATA_PATCH_V1";

const UPSTREAM_IMPORT = 'import { NodeHtmlMarkdown } from "node-html-markdown";';
const PATCH_IMPORT = 'import { extractPageDateMetadata } from "../../../patches/page-date.mjs";';
const UPSTREAM_CONVERSION = '    return { kind: "markdown", text: convertText(read.text, type, attempt.url) };';
const PATCHED_CONVERSION = [
  '    const pageDateMetadata = type.kind === "html" ? extractPageDateMetadata(read.text) : "";',
  '    const markdown = convertText(read.text, type, attempt.url);',
  '    return { kind: "markdown", text: pageDateMetadata ? `${pageDateMetadata}\\n\\n${markdown}` : markdown };',
].join("\n");

export function patchUrlReader(source) {
  if (source.includes(PATCH_MARKER)) {
    if (!source.includes(PATCH_IMPORT) || !source.includes("extractPageDateMetadata(read.text)")) {
      throw new Error("The existing Morde page-date patch is incomplete; refusing to continue.");
    }
    return source;
  }
  if (!source.includes(UPSTREAM_IMPORT) || !source.includes(UPSTREAM_CONVERSION)) {
    throw new Error("The pinned mcp-searxng URL reader differs from the expected 2.10.0 source; refusing to patch.");
  }
  return source
    .replace(UPSTREAM_IMPORT, `${UPSTREAM_IMPORT}\n${PATCH_IMPORT} // ${PATCH_MARKER}`)
    .replace(UPSTREAM_CONVERSION, PATCHED_CONVERSION);
}

export async function applyPatch(packageDir) {
  const packageRoot = path.resolve(packageDir);
  const helperPath = path.join(path.dirname(path.dirname(packageRoot)), "patches", "page-date.mjs");
  const helper = await readFile(helperPath, "utf8");
  if (!helper.includes("export function extractPageDateMetadata")) {
    throw new Error("The Morde page-date helper is missing or invalid.");
  }
  const packagePath = path.join(packageRoot, "package.json");
  const metadata = JSON.parse(await readFile(packagePath, "utf8"));
  if (metadata.name !== "mcp-searxng" || metadata.version !== "2.10.0") {
    throw new Error("The Morde patch only supports mcp-searxng 2.10.0.");
  }
  const readerPath = path.join(packageRoot, "dist", "url-reader.js");
  const source = await readFile(readerPath, "utf8");
  const patched = patchUrlReader(source);
  if (patched !== source) await writeFile(readerPath, patched, "utf8");
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const scriptDir = path.dirname(fileURLToPath(import.meta.url));
  const packageDir = process.argv[2] ?? path.join(scriptDir, "node_modules", "mcp-searxng");
  try {
    await applyPatch(packageDir);
    process.stdout.write(`Applied ${PATCH_MARKER} to mcp-searxng 2.10.0.\n`);
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
