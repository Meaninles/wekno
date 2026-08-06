import { cp, mkdir, rm } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const moduleDirectory = dirname(fileURLToPath(import.meta.url));
const frontendRoot = resolve(moduleDirectory, "../../../..");
const pdfJsRoot = resolve(frontendRoot, "node_modules/pdfjs-dist");
const outputRoot = resolve(frontendRoot, "public/pdfjs");
const assetDirectories = ["cmaps", "standard_fonts", "wasm", "iccs"];

await rm(outputRoot, { recursive: true, force: true });
await mkdir(outputRoot, { recursive: true });

for (const directory of assetDirectories) {
  await cp(resolve(pdfJsRoot, directory), resolve(outputRoot, directory), {
    recursive: true,
  });
}
