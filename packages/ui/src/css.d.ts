/**
 * The shared components import their stylesheet (`import "./ui.css"`),
 * which Next's compiler resolves and extracts. TypeScript has no such
 * loader, so this stands in for it — without it `tsc --noEmit` fails on
 * the import itself and `pnpm --filter @post/ui typecheck` cannot run.
 */
declare module "*.css";
