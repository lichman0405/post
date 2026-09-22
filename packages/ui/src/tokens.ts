/**
 * The POST design-token manifest (docs/41_DESIGN_TOKENS.md:6-12).
 *
 * # Why this file exists at all
 *
 * Before T1101 every surface wrote its own bare values: `#59636e` appeared
 * 162 times, `#d0d7de` 84, and the same Primer functional token was
 * re-spelled per page. The values were never wrong — they were
 * re-discovered, so a change to the palette had nowhere to land. This
 * module is the one place the roles of docs/41 are named.
 *
 * # The role names are the spec's, verbatim
 *
 * docs/41:6-12 lists exactly fourteen colour roles, and this object has
 * exactly those fourteen keys — nothing added. docs/41:14 forbids inventing
 * tokens (`brandGradient`, `purpleGlow`, `glassBackground` and their
 * relatives); a manifest that quietly grew a fifteenth role would be the
 * same mistake with a better conscience. Section B of
 * `tests/web-smoke/visual-regression.mjs` reads this file and fails when
 * the key set is not exactly the spec's fourteen, in the spec's order —
 * and section B runs on every baseline comparison, so a role added here
 * cannot land without a red.
 *
 * # What each value is
 *
 * A CSS expression, not a literal: the Primer functional token first, its
 * light-theme hex as the fallback. That is the shape the whole app already
 * uses (`var(--fgColor-muted, #59636e)`), kept rather than replaced — a
 * custom component still renders correctly if the Primer token set is not
 * on the page, and a reader can see the intended colour without chasing
 * the stylesheet.
 *
 * # What is deliberately NOT here
 *
 * Radius, spacing and type scale are docs/41 sections too, but they are
 * not *tokens* in the spec's sense and adding them would grow the manifest
 * past the fourteen roles. They live in the shared stylesheet
 * (`ui.css`) as the component geometry they belong to.
 *
 * The tinted backgrounds and borders a label needs (`--bgColor-*-muted`,
 * `--borderColor-*-muted`) are read from Primer by that same stylesheet.
 * docs/41's `*-emphasis` roles are solid fills and would make a badge
 * unreadable as a background, so they name the accent/emphasis *colour*,
 * not every surface that carries it.
 */

export const tokens = {
  "canvas.default": "var(--bgColor-default, #ffffff)",
  "canvas.subtle": "var(--bgColor-muted, #f6f8fa)",
  "fg.default": "var(--fgColor-default, #1f2328)",
  "fg.muted": "var(--fgColor-muted, #59636e)",
  "border.default": "var(--borderColor-default, #d0d7de)",
  "border.muted": "var(--borderColor-muted, #d8dee4)",
  "accent.fg": "var(--fgColor-accent, #0969da)",
  "accent.emphasis": "var(--bgColor-accent-emphasis, #0969da)",
  "success.fg": "var(--fgColor-success, #1a7f37)",
  "success.emphasis": "var(--bgColor-success-emphasis, #1f883d)",
  "attention.fg": "var(--fgColor-attention, #9a6700)",
  "attention.emphasis": "var(--bgColor-attention-emphasis, #9a6700)",
  "danger.fg": "var(--fgColor-danger, #d1242f)",
  "danger.emphasis": "var(--bgColor-danger-emphasis, #cf222e)",
} as const;

/** The spec's fourteen role names, as a type. */
export type TokenName = keyof typeof tokens;

/** The role name -> CSS expression map. */
export type TokenManifest = typeof tokens;

/**
 * The five colour families shared components express state in — the
 * `neutral` family is `fg.muted` + `border.default`, the other four are
 * the spec's `*.fg` roles. Exported so a component's `tone` prop, a page's
 * label, and the stylesheet's modifier classes cannot drift apart.
 */
export type Tone = "neutral" | "accent" | "success" | "attention" | "danger";

/** The CSS class suffix each tone renders as (`post-<kind>-<tone>`). */
export const tones: readonly Tone[] = [
  "neutral",
  "accent",
  "success",
  "attention",
  "danger",
];
