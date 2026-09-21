#!/usr/bin/env node
/**
 * Static check: no ok() call in this directory passes a second argument.
 *
 * WHY THIS EXISTS NEXT TO THE RUNTIME GUARD. `ok` itself now fails the run
 * when it is handed two arguments (see visual-smoke.mjs), which catches every
 * bad call that EXECUTES. That is not quite the same as "no bad call exists":
 * a two-argument call left inside a branch this run does not take would print
 * nothing and be found by nobody. T1112's whole premise is that the shape is
 * invisible until something goes red on it, so the shape is checked
 * statically as well — here, over the file text, before a browser is launched.
 *
 * This is also the instrument behind T1112's "the count is zero" evidence.
 * The obvious grep for it — `grep -E '\bok\([^)]*,'` — reports seven hits on a
 * clean tree, every one of them a false positive: template-literal labels
 * containing a comma (`ok(\`${a.join(", ")}\`)`), the guard's own
 * `function ok(label, ...extra)` signature, and a comment discussing the old
 * bug. A check whose honest output needs seven footnotes is not a check, so
 * the commas are counted only where they are real: after comments and string
 * bodies have been blanked out, and only at paren depth 0 inside the call.
 *
 * Usage: node check-ok-arity.js        (exit 0 = clean, 1 = a bad call exists)
 */
import fs from "node:fs";
import path from "node:path";

const DIR = import.meta.dirname;

/** Blank comments and string/template literals, preserving offsets and lines. */
function blankLiterals(src) {
  let out = "";
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === "/" && src[i + 1] === "/") {
      while (i < src.length && src[i] !== "\n") {
        out += " ";
        i += 1;
      }
      continue;
    }
    if (c === "/" && src[i + 1] === "*") {
      out += "  ";
      i += 2;
      while (i < src.length && !(src[i] === "*" && src[i + 1] === "/")) {
        out += src[i] === "\n" ? "\n" : " ";
        i += 1;
      }
      out += "  ";
      i += 2;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") {
      const quote = c;
      out += " ";
      i += 1;
      while (i < src.length) {
        if (src[i] === "\\") {
          out += "  ";
          i += 2;
          continue;
        }
        if (src[i] === quote) {
          out += " ";
          i += 1;
          break;
        }
        out += src[i] === "\n" ? "\n" : " ";
        i += 1;
      }
      continue;
    }
    out += c;
    i += 1;
  }
  return out;
}

const lineAt = (src, offset) => src.slice(0, offset).split("\n").length;

const files = fs.readdirSync(DIR).filter((f) => f.endsWith(".mjs")).sort();
if (files.length === 0) {
  console.log("ok-arity: FAILED — no *.mjs files in this directory; the check measured nothing.");
  process.exit(1);
}

let examined = 0;
const offenders = [];
for (const file of files) {
  const raw = fs.readFileSync(path.join(DIR, file), "utf8");
  const code = blankLiterals(raw);
  const call = /\bok\s*\(/g;
  let m;
  while ((m = call.exec(code)) !== null) {
    // The definition site is not a call: `function ok(label, ...extra)`.
    if (/\bfunction\s+$/.test(code.slice(0, m.index))) continue;
    examined += 1;
    let depth = 0;
    let commas = 0;
    for (let j = call.lastIndex; j < code.length; j += 1) {
      const ch = code[j];
      if (ch === "(" || ch === "[" || ch === "{") depth += 1;
      else if (ch === ")" || ch === "]" || ch === "}") {
        if (depth === 0) break;
        depth -= 1;
      } else if (ch === "," && depth === 0) commas += 1;
    }
    if (commas > 0) {
      offenders.push(`${file}:${lineAt(raw, m.index)} passes ${commas + 1} argument(s) to ok()`);
    }
  }
}

console.log(`ok-arity: examined ${examined} ok() call(s) in ${files.length} file(s): ${files.join(", ")}`);
// A scanner that matched nothing would report "clean" without having looked.
// Measured 2026-09-21, after T1112's rewrites: 39 calls across the two smoke
// files. The floor is deliberately well under that so ordinary edits do not
// trip it, but a scanner that sees almost nothing says so instead of passing.
if (examined < 20) {
  console.log(`ok-arity: FAILED — only ${examined} ok() call(s) found; this scanner is not seeing the file it claims to check.`);
  process.exit(1);
}
if (offenders.length > 0) {
  console.log("ok-arity: FAILED — these calls hand ok() a second argument, which ok discards:");
  for (const o of offenders) console.log(`  ${o}`);
  console.log("Write the condition as an if/else that calls fail(label, detail), or drop the line.");
  process.exit(1);
}
console.log("ok-arity: 0 ok() call(s) pass a second argument");
