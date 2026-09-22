/**
 * WHERE the pixels moved (T1101 AC3 evidence).
 *
 * visual-regression.mjs ends at a count. "3577 pixels differ" says how bad
 * and to which page, never to which ELEMENT — and the rework letter asks the
 * two authorized exceptions for their pixel locations, because a reviewer
 * reading "652 pixels" has to be able to see that they land on the
 * pull-detail radius they authorized, and not on some other page's badge.
 *
 * This reads the same two images the regression run reads, with the same
 * perceptual threshold, and prints every 8-connected region of differing
 * pixels as count + bounding box. The red-channel filter is not a
 * re-implementation of pixelmatch: it reads pixelmatch's own diff image,
 * where a counted difference is drawn in the diff colour (255,0,0) and an
 * anti-aliased pixel that did NOT count is drawn in the AA colour
 * (255,255,0). The run asserts the two agree — `total` from the counter vs
 * the pixels this locator found — so a locator that quietly measured
 * something else would fail rather than print a confident table.
 *
 * Usage: node diff-locations.mjs <baseline.png> <current.png> [maxClusters]
 */
import fs from "node:fs";
import pixelmatch from "pixelmatch";
import { PNG } from "pngjs";

const [baselinePath, currentPath, maxClustersArg] = process.argv.slice(2);
if (!baselinePath || !currentPath) {
  console.error("usage: node diff-locations.mjs <baseline.png> <current.png> [maxClusters]");
  process.exit(2);
}
const maxClusters = Number(maxClustersArg ?? 25);

/** The same number visual-regression.mjs compares with. */
const PERCEPTUAL_THRESHOLD = 0.1;

const baseline = PNG.sync.read(fs.readFileSync(baselinePath));
const current = PNG.sync.read(fs.readFileSync(currentPath));

/* A page that changed HEIGHT cannot be compared pixel-for-pixel by the
 * regression run — it fails on the size alone and stops there, which is why
 * `project-conflicts` shows up as a size change rather than a number. For
 * the two authorized exceptions a number is exactly what is wanted, so this
 * compares the OVERLAP (top-left, both images at their own origin) and says
 * loudly that the rows past the shorter image are unmeasured. It never
 * resizes: scaling one image onto the other would manufacture a match. */
const width = Math.min(baseline.width, current.width);
const height = Math.min(baseline.height, current.height);
if (baseline.width !== current.width || baseline.height !== current.height) {
  console.log(
    `SIZE DIFFERS: baseline ${baseline.width}x${baseline.height} vs current ${current.width}x${current.height}; ` +
      `comparing the ${width}x${height} overlap only`,
  );
}

/** Crop a PNG's RGBA buffer to the top-left w×h, without scaling. */
function crop(png, w, h) {
  if (png.width === w && png.height === h) return png.data;
  const out = Buffer.alloc(w * h * 4);
  for (let y = 0; y < h; y += 1) {
    png.data.copy(out, y * w * 4, y * png.width * 4, y * png.width * 4 + w * 4);
  }
  return out;
}

const diff = new PNG({ width, height });
const total = pixelmatch(crop(baseline, width, height), crop(current, width, height), diff.data, width, height, {
  threshold: PERCEPTUAL_THRESHOLD,
});

/** pixelmatch's counted-difference colour. */
const isDiffPixel = (i) => diff.data[i] === 255 && diff.data[i + 1] === 0 && diff.data[i + 2] === 0 && diff.data[i + 3] === 255;

const marked = new Uint8Array(width * height);
let found = 0;
let allMinX = width, allMaxX = -1, allMinY = height, allMaxY = -1;
for (let y = 0; y < height; y += 1) {
  for (let x = 0; x < width; x += 1) {
    const p = y * width + x;
    if (isDiffPixel(p * 4)) {
      marked[p] = 1;
      found += 1;
      if (x < allMinX) allMinX = x;
      if (x > allMaxX) allMaxX = x;
      if (y < allMinY) allMinY = y;
      if (y > allMaxY) allMaxY = y;
    }
  }
}

console.log(`${baselinePath} vs ${currentPath}`);
console.log(`${width}x${height}; counted ${total} (threshold ${PERCEPTUAL_THRESHOLD}), located ${found}`);

if (found !== total) {
  // Refuse to print locations for a set of pixels the counter did not count.
  console.log(`LOCATOR MISMATCH: the locator and the counter disagree — locations below are not trustworthy`);
  process.exit(1);
}
if (total === 0) {
  console.log("no differing pixel");
  process.exit(0);
}

/* 8-connected flood fill. BFS over a typed array and an explicit stack: the
 * counts here are thousands, not millions, and recursion would be the only
 * way to make this fragile. */
const clusters = [];
const stack = [];
for (let start = 0; start < marked.length; start += 1) {
  if (marked[start] !== 1) continue;
  marked[start] = 2;
  stack.push(start);
  let count = 0;
  let minX = width, maxX = -1, minY = height, maxY = -1;
  while (stack.length > 0) {
    const p = stack.pop();
    const x = p % width;
    const y = (p - x) / width;
    count += 1;
    if (x < minX) minX = x;
    if (x > maxX) maxX = x;
    if (y < minY) minY = y;
    if (y > maxY) maxY = y;
    for (let dy = -1; dy <= 1; dy += 1) {
      for (let dx = -1; dx <= 1; dx += 1) {
        const nx = x + dx;
        const ny = y + dy;
        if (nx < 0 || ny < 0 || nx >= width || ny >= height) continue;
        const np = ny * width + nx;
        if (marked[np] === 1) {
          marked[np] = 2;
          stack.push(np);
        }
      }
    }
  }
  clusters.push({ count, minX, maxX, minY, maxY });
}

clusters.sort((a, b) => b.count - a.count);
console.log(
  `overall bounding box: x ${allMinX}..${allMaxX}, y ${allMinY}..${allMaxY} — ` +
    `nothing differs above y=${allMinY}`,
);
console.log(`${clusters.length} cluster(s); showing up to ${maxClusters}, largest first`);
for (const c of clusters.slice(0, maxClusters)) {
  const w = c.maxX - c.minX + 1;
  const h = c.maxY - c.minY + 1;
  console.log(
    `  ${String(c.count).padStart(6)} px  x ${String(c.minX).padStart(4)}..${String(c.maxX).padEnd(4)} ` +
      `y ${String(c.minY).padStart(4)}..${String(c.maxY).padEnd(4)}  (${w}x${h} box)`,
  );
}
// The full-page viewport is 1280 wide: a cluster that starts in the 1180s is
// in the right-hand margin, one in the 20s is in the main column. Print the
// column hint once so the reader does not have to hold the geometry in mind.
console.log("column hint: main column starts near x=24, right rail near x=940, page margin past x=1180");
