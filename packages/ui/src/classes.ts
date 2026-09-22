/**
 * Join class names, dropping anything falsy.
 *
 * The shared components are used from plain CSS-class surfaces (this app
 * has no CSS-in-JS), so every one of them needs the same three-way join:
 * its own base class, one modifier per variant, and the page's own class
 * when a caller needs to place the element. A caller's `className` is
 * always LAST, so a page rule can override a shared rule of equal
 * specificity the ordinary way rather than by using `!important`.
 */
export function cx(...parts: Array<string | false | null | undefined>): string {
  return parts.filter((p): p is string => Boolean(p)).join(" ");
}
