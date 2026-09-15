# Reading quota data

The TUI does not assume each plan has every window. Codex account/rateLimits/read
can return a weekly primary window and a null secondary window. Window duration,
not the primary/secondary name, determines the column. Separate Spark windows are
reported under codex_bengalfox. Current OpenAI documentation describes Spark as a
Pro-only research preview with its own limit:
https://help.openai.com/en/articles/11481834-cha

No window means "Not reported", not 0% used or unlimited. A malformed or absent
percentage is excluded rather than coerced to zero. HTTP 401/token_revoked is an
authentication failure. An HTTP 429 from the Claude usage endpoint is a failed,
throttled lookup, not evidence that the user's allowance is exhausted. The TUI
caches successful quota reads for 60 seconds and increases its cooldown after
repeated 429 responses, respecting Retry-After. Last good data retains its real
read timestamp and is explicitly marked cached.

Stored credential profiles are not unique account identities. The TUI groups only
matching provider, email and workspace triples, prefers a successful profile for
actions, and retains aliases and other-profile errors in details. It never removes
profiles or combines different workspace balances.

Reset credits are a separate read-only request. A confirmed zero, a failed lookup,
and a non-Codex account are three different states. No reset is bought or redeemed.
