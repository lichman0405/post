import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // @post/ui ships TypeScript source (docs/65: shared web/ui components live
  // in the pnpm workspace, compiled by Next).
  transpilePackages: ["@post/ui"],
};

export default nextConfig;
