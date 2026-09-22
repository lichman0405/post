"use client";

import { Select } from "@primer/react";

import { useLocale } from "../i18n-provider";
import {
  LOCALE_COOKIE,
  LOCALE_COOKIE_MAX_AGE,
  LOCALES,
  translate,
  type Locale,
} from "../../lib/i18n";

/**
 * The language preference control (T1105) — the only way a reader changes
 * their language, and the reason the rest of this task is observable.
 *
 * A native `<select>` and not a menu: it is the one control whose keyboard
 * behaviour, screen-reader announcement and mobile presentation the platform
 * already gets right, and a custom listbox would be a re-implementation of
 * exactly those three things for no gain. It is also the shape a browser
 * suite can drive without guessing at ARIA.
 *
 * The accessible name is `nav.language`, localized ("Language" / "语言"),
 * and it is carried on the select itself with `aria-label` rather than by a
 * wrapping `<label>`: the header has no room for a visible one, and a
 * wrapping label would make the control's accessible name the concatenation
 * of its option texts.
 *
 * The option labels are each language's own endonym — "English" and
 * "简体中文" — in BOTH locales. That is deliberate and it is the one place
 * in this app where a string does not change with the locale: the reader who
 * needs the switcher is by definition looking at a page they cannot read, so
 * the entry they are looking for has to be written the way they write it.
 *
 * THE SWITCH IS A FULL DOCUMENT LOAD, and that is a decision rather than a
 * shrug. `<html lang>` is rendered by the server from this same cookie (see
 * app/layout.tsx). A soft refresh re-renders the React tree but the document
 * element's own attributes are the browser's, not React's, in the paths that
 * matter here — so the one attribute WCAG 3.1.1 is about could lag behind
 * the text it describes, which is worse than the reload it saves. A
 * language change is a rare, deliberate act; one page load is the honest
 * price of the document and its strings agreeing from the first byte.
 */
const LOCALE_LABEL_KEY: Record<Locale, string> = {
  en: "locale.name.en",
  "zh-CN": "locale.name.zh-CN",
};

export function LanguageSwitcher() {
  const locale = useLocale();
  const label = translate(locale, "nav.language");

  function choose(next: string) {
    document.cookie = `${LOCALE_COOKIE}=${encodeURIComponent(next)}; path=/; max-age=${LOCALE_COOKIE_MAX_AGE}; samesite=lax`;
    window.location.reload();
  }

  return (
    <Select
      className="global-nav-language"
      data-language-switcher
      aria-label={label}
      value={locale}
      onChange={(event) => choose(event.target.value)}
    >
      {LOCALES.map((candidate) => (
        <option key={candidate} value={candidate}>
          {translate(candidate, LOCALE_LABEL_KEY[candidate])}
        </option>
      ))}
    </Select>
  );
}
