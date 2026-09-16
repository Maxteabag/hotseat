"""Build snapshots on demand, sharing cached data and in-flight refreshes."""

from __future__ import annotations

from concurrent.futures import Future, ThreadPoolExecutor
import threading
import time

from . import plugins, codex, modelstats, ratelimits, resume, usage, refresh
from .backends import Backend, BackendError, for_platform
from .sessions import running_sessions

REFRESH_INTERVAL_S = 300
MAX_PARALLEL_PROBES = 4
#: Warn this far ahead of the hard interactive-sign-in deadline.
SIGNIN_WARN_DAYS = 7


def _account_view(account, probe_result: dict | None, error: str | None) -> dict:
    now = time.time()
    view = account.public()
    view["error"] = error
    view["usage"] = probe_result

    access = account.access_expires_at
    refresh = account.refresh_expires_at
    view["access_hours_left"] = (access / 1000 - now) / 3600 if access else None
    # Refreshing renews the access token but does not extend this window, so it
    # counts down to a hard browser sign-in no matter how much the account is used.
    view["signin_days_left"] = (refresh / 1000 - now) / 86400 if refresh else None
    view["signin_due_soon"] = bool(
        view["signin_days_left"] is not None and view["signin_days_left"] <= SIGNIN_WARN_DAYS)
    return view


class Collector:
    """Owns the cached snapshot and coordinates concurrent refresh requests."""

    def __init__(self, backend: Backend | None = None, interval: int = REFRESH_INTERVAL_S) -> None:
        self.backend = backend or for_platform()
        self.interval = interval
        self._lock = threading.Lock()
        self._snapshot: dict | None = None
        self._inflight: Future | None = None

    # --- building ---------------------------------------------------------
    def build(self) -> dict:
        """Probe every account and return a fresh snapshot."""
        try:
            accounts = self.backend.accounts()
            fatal = None
        except BackendError as exc:
            accounts, fatal = [], str(exc)

        views: list[dict] = []
        if accounts:
            with ThreadPoolExecutor(max_workers=MAX_PARALLEL_PROBES) as pool:
                results = list(pool.map(self._probe, accounts))
            views = [_account_view(a, usage, error) for a, usage, error in results]

        return {
            "generated_at": time.time(),
            "capabilities": self.backend.capabilities(),
            "sessions": running_sessions(),
            "accounts": views,
            # Machine-wide and token-based. Deliberately not merged into the
            # per-account quota above: different source, different unit.
            "model_usage": modelstats.recent_by_model(),
            "extensions": plugins.snapshot(views),
            "codex": self._codex(),
            "stopped": self._stopped(views),
            "error": fatal,
        }


    @staticmethod
    def _codex() -> dict | None:
        """Codex accounts. Absent is a normal state, not an error.

        Quota comes from cache here. Probing spawns one process per saved account,
        which is not something a refresh timer should do; the windows are hourly
        and weekly, so a reading up to half an hour old is still useful.
        """
        try:
            return codex.overview(max_age=codex.LIMITS_TTL_S)
        except codex.CodexError:
            return None

    @staticmethod
    def _stopped(views: list[dict]) -> list[dict]:
        """Work a usage limit stopped, with whether it can be continued now."""
        try:
            items = resume.stopped()
        except resume.ResumeError:
            return []
        default = next((v["alias"] for v in views if v.get("is_active")), None)
        for item in items:
            item["readiness"] = resume.readiness(item, views, default)
        return items

    def _probe(self, account):
        """Read one account's quota.

        The OAuth usage endpoint is the real source: it costs no inference, answers
        even when the account is rate limited, and reports per-model limits. The
        header probe is kept only as a fallback for when that endpoint is
        unavailable, and it cannot see per-model limits at all.
        """
        if not account.token:
            return account, None, "no stored token"
        if refresh.expired(account):
            # Never send a dead token to the API. Bring the profile back through
            # the CLI first; if that cannot be done, say why and stop there.
            try:
                refresh.auto(self.backend, account)
            except Exception as exc:  # RefreshError, or anything unexpected underneath it
                return account, None, f"Access token expired; refresh failed: {exc}"
        try:
            return account, usage.for_token(account.token), None
        except usage.UsageError as first:
            try:
                fallback = ratelimits.summarise(ratelimits.probe(account.token))
            except ratelimits.ProbeError:
                return account, None, str(first)
            fallback["scoped"] = []
            fallback["blocked_models"] = []
            fallback["degraded"] = "per-model limits unavailable"
            return account, fallback, None

    # --- cache ------------------------------------------------------------
    def snapshot(self, max_age: float | None = None) -> dict:
        """Return the snapshot, rebuilding it only if the cached one is too old.

        Work happens when something asks for it. Refreshing on a timer means
        probing accounts and scanning transcripts for nobody whenever the page is
        closed, which is most of the time.
        """
        limit = self.interval if max_age is None else max_age
        return self._get_snapshot(limit)

    def _get_snapshot(self, max_age: float, force: bool = False) -> dict:
        with self._lock:
            cached = self._snapshot
            if not force and cached is not None and (
                    time.time() - cached.get("generated_at", 0)) <= max_age:
                return cached
            future = self._inflight
            owner = future is None
            if owner:
                future = self._inflight = Future()
        if not owner:
            return future.result()
        try:
            fresh = self.build()
            with self._lock:
                self._snapshot = fresh
            future.set_result(fresh)
            return fresh
        except BaseException as exc:
            future.set_exception(exc)
            raise
        finally:
            with self._lock:
                self._inflight = None

    def refresh(self) -> dict:
        return self._get_snapshot(0, force=True)

    def stop(self) -> None:
        pass  # Collection is request-driven; there is no worker to stop.
