import type { Metadata } from "next";
import { Providers } from "./providers";
import { getLocale, getT } from "../lib/i18n-server";
import "./globals.css";

/**
 * T1105: the document title and description are user-visible copy (they are
 * the browser tab and the link preview), so they come from the catalog too.
 * `generateMetadata` rather than the `metadata` constant because the locale
 * is only known per request — see the layout below for where it is read.
 */
export async function generateMetadata(): Promise<Metadata> {
  const { t } = await getT();
  return {
    title: t("shell.siteTitle"),
    description: t("shell.siteDescription"),
  };
}

/**
 * The root layout, and the one place the language preference is read
 * (T1105, docs/28 §3).
 *
 * `<html lang>` was hardcoded to "en" until this task. That is a WCAG 3.1.1
 * (Language of Page) defect the moment the page can be rendered in another
 * language: a screen reader picks its pronunciation from this attribute, so
 * a Chinese page announcing `lang="en"` is read out with English phonetics.
 * The attribute therefore has to come from the same source as the strings,
 * which is why the cookie is read here and not in a component.
 *
 * Reading a cookie opts the routes under this layout into dynamic rendering
 * — a page whose output depends on a request cannot be prerendered once and
 * shared. That is a real cost and it is the price of a server-rendered
 * `lang`: the alternative is a static shell that ships `lang="en"` and
 * corrects it in the browser, which is wrong exactly for the reader who
 * depends on it. Every page here already fetches with `cache: "no-store"`
 * against a live API, so nothing that was meaningfully static becomes
 * dynamic.
 *
 * The T1104 investigation explicitly deferred this attribute to this task
 * (it recorded "不改、只记录"), because the line has to change together with
 * the preference that decides it — otherwise the two tasks edit it twice.
 */
export default async function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  const locale = await getLocale();
  return (
    <html lang={locale}>
      <body>
        <Providers locale={locale}>{children}</Providers>
      </body>
    </html>
  );
}
