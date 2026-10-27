/**
 * Tiny static server for the WEB-01 negative-case fixtures.
 *
 * It serves the two checked-in HTML fixtures from scripts/tests/fixtures so
 * the entry-point checker can be shown to FAIL a page without an
 * account-establishment entry and PASS a complete one. It serves no API and
 * performs no business step; the real product is exercised only by
 * tests/web-v2/run.sh against real services.
 *
 * Usage: node scripts/tests/web-v2-fixture-server.mjs <fixtures-dir> <port>
 */

import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";

const dir = process.argv[2];
const port = Number(process.argv[3] ?? "18080");
if (!dir || !Number.isInteger(port) || port <= 0) {
  console.error("web-v2 fixture server: usage: node web-v2-fixture-server.mjs <fixtures-dir> <port>");
  process.exit(2);
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);
  const name = path.basename(url.pathname);
  if (!name.endsWith(".html")) {
    res.writeHead(404, { "content-type": "text/plain" });
    res.end("not found");
    return;
  }
  try {
    const body = await readFile(path.join(dir, name));
    res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
    res.end(body);
  } catch {
    res.writeHead(404, { "content-type": "text/plain" });
    res.end("not found");
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`web-v2 fixture server: listening on http://127.0.0.1:${port}`);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => server.close(() => process.exit(0)));
}
