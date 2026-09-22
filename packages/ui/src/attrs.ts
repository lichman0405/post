/**
 * Attributes a shared component's element can carry.
 *
 * React's `HTMLAttributes` admits the attributes React itself knows and
 * rejects unknown keys in an object literal, so a caller cannot write
 * `rowAttrs={(m) => ({ "data-member-row": m.user_id })}` — TypeScript
 * reports "no properties in common". But those markers are exactly how
 * this app's e2e harnesses select: `[data-member-row]`, `[data-pull-row]`,
 * `[data-side]`, `[data-activity-*]`, `[data-conflicts-clean]`.
 *
 * T1101's components therefore keep the caller's escape hatch explicitly
 * typed instead of resorting to `any` (which would also stop checking the
 * real attributes) or dropping the markers (which would have silently
 * broken four e2e checklists). The `data-` prefix is enforced by the
 * template-literal index signature: an arbitrary key is still an error.
 */
export type DataAttributes = {
  [key: `data-${string}`]: string | number | boolean | undefined;
};

/** An element's own props, plus its `data-*` markers. */
export type WithData<T> = T & DataAttributes;
