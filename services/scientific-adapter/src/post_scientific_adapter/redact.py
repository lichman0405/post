"""Output redaction for the scientific adapter (T0004 baseline).

Mirrors ``scripts/speclib.redact_url()`` exactly: credential-bearing URLs are
masked before they ever reach stdout, stderr, logs or error messages.
"""

from __future__ import annotations


def redact_url(url: str) -> str:
    """Strip embedded credentials from a URL before it is ever emitted.

    ``https://x-access-token:ghp_xxx@github.com/...`` and
    ``postgres://user:pw@host/db`` are credential-bearing forms that must
    not reach output. Only ``scheme://userinfo@host`` is rewritten;
    scp-like ``git@host:path`` keeps its conventional user and is left
    alone. Returns the input unchanged when it carries no userinfo.

    ``user:secret@host`` becomes ``user:***@host`` (keeps the user, drops
    the secret); token-only userinfo becomes ``***@host``. This mirrors
    ``scripts/speclib.redact_url()`` exactly (T0004 precedent).
    """
    if not isinstance(url, str):
        return url
    if "://" not in url:
        return url
    scheme, rest = url.split("://", 1)
    if "@" not in rest:
        return url
    # Split at the LAST '@': a password may itself contain '@'.
    userinfo, hostpart = rest.rsplit("@", 1)
    if not userinfo:
        return url
    if ":" in userinfo:
        user = userinfo.split(":", 1)[0]
        return f"{scheme}://{user}:***@{hostpart}"
    return f"{scheme}://***@{hostpart}"
