/**
 * Honest placeholder for project tabs whose product surface lands in a
 * later milestone (T0108 ships the shell and its navigation, not the
 * research workflow behind every tab). Each placeholder names what
 * replaces it, so the gap is discoverable instead of a dead end — the
 * same convention as the global-nav ComingSoon stubs.
 */
export function TabPlaceholder({
  title,
  milestone,
  children,
}: {
  title: string;
  milestone: string;
  children: React.ReactNode;
}) {
  return (
    <div className="tab-placeholder" data-tab-placeholder={title}>
      <h2 className="tab-placeholder-title">{title}</h2>
      <p className="tab-placeholder-desc">
        {children} This area lands with <strong>{milestone}</strong>.
      </p>
    </div>
  );
}
