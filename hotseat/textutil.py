"""Turning stored conversation text into something readable in a list or terminal.

Shared by the stopped-work listing and the inspector so both clean and shorten text
the same way, and so neither has to import the other.
"""

from __future__ import annotations

import re

#: Voice clients wrap spoken replies in TTS markup that is noise when read.
SPEECH_MARKUP = re.compile(r"</?(speak|break|voice|speed|emphasis|prosody|phoneme)[^>]*>")
SNIPPET = 400


def clean(text: str | None) -> str:
    if not text:
        return ""
    return " ".join(SPEECH_MARKUP.sub(" ", text).split())


def trim(text: str | None, limit: int = SNIPPET) -> str:
    """Clean and shorten text for display."""
    cleaned = clean(text)
    return cleaned if len(cleaned) <= limit else cleaned[:limit].rstrip() + "…"


def entry_text(entry: dict) -> str:
    """The readable text of one native transcript entry."""
    content = (entry.get("message") or {}).get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return " ".join(part.get("text", "") for part in content
                        if isinstance(part, dict) and part.get("type") == "text")
    return ""
