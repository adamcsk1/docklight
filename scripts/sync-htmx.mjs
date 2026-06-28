import { copyFile, mkdir, readFile } from "node:fs/promises";
import { dirname, join } from "node:path";

const source = join("node_modules", "htmx.org", "dist", "htmx.min.js");
const target = join("cmd", "docklight", "static", "htmx.min.js");

const packageJson = JSON.parse(await readFile("package.json", "utf8"));
const dependencyVersion = packageJson.dependencies?.["htmx.org"];
if (!dependencyVersion) {
  throw new Error("package.json is missing dependencies.htmx.org");
}
const expectedVersion = dependencyVersion.replace(/^[~^]/, "");

const content = await readFile(source, "utf8");
const match = content.match(/version:\s*"([^"]+)"/);
if (!match) {
  throw new Error(`Unable to find HTMX version in ${source}`);
}
if (match[1] !== expectedVersion) {
  throw new Error(`HTMX version mismatch: package.json has ${dependencyVersion}, ${source} has ${match[1]}`);
}

await mkdir(dirname(target), { recursive: true });
await copyFile(source, target);
console.log(`Synced htmx.org ${expectedVersion} to ${target}`);
