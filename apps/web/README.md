# @post/web

POST web application: Next.js 16 Active LTS + React + strict TypeScript +
Primer/Octicons (docs/68, ADR-012, ADR-018).

- **No backend here.** This app renders pages and fetches the Go API over
  HTTP from the server side (`app/page.tsx`). There are deliberately no route
  handlers, server actions or API routes acting as a second backend
  (docs/51, ADR-018).
- Shared UI components live in `@post/ui` (pnpm workspace, transpiled by
  Next via `transpilePackages`).
- Commands: `pnpm dev` · `pnpm build` · `pnpm typecheck` · `pnpm lint`.
  The root `make check` runs typecheck + lint as part of the cross-language
  one-command check.
