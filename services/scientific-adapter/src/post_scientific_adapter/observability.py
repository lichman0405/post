"""Structured request logging for the scientific adapter (T0007).

Stdlib logging with a JSON formatter: every request line is one JSON object
carrying time, level, message and the request's correlation id. The
correlation id travels via the ``extra`` record field — the formatter never
inspects messages, so a credential-shaped value in a message cannot be
mistaken for a trace id.

The adapter reuses its existing redaction (redact.py) for URL-shaped values
on any output path — there is no second redactor here.
"""

from __future__ import annotations

import json
import logging
import sys
from datetime import datetime, timezone
from typing import IO

REQUEST_LOGGER = "post.scientific.adapter"


class JsonFormatter(logging.Formatter):
    """One JSON object per log record; correlation_id via extra."""

    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, object] = {
            "time": datetime.now(timezone.utc).isoformat(timespec="milliseconds"),
            "level": record.levelname.lower(),
            "msg": record.getMessage(),
        }
        correlation_id = getattr(record, "correlation_id", "")
        if correlation_id:
            payload["correlation_id"] = correlation_id
        if record.exc_info:
            payload["error"] = self.formatException(record.exc_info)
        return json.dumps(payload, ensure_ascii=False)


def configure_request_logging(stream: IO[str] = sys.stderr) -> None:
    """Attach the JSON handler to the adapter's request logger.

    Idempotent: repeated calls (tests, embedded use) replace the handler
    instead of stacking duplicates.
    """
    logger = logging.getLogger(REQUEST_LOGGER)
    logger.setLevel(logging.INFO)
    logger.propagate = False
    for handler in list(logger.handlers):
        logger.removeHandler(handler)
    handler = logging.StreamHandler(stream)
    handler.setFormatter(JsonFormatter())
    logger.addHandler(handler)
