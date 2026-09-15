"""Shared shape for the per-platform credential backends."""

from __future__ import annotations

from dataclasses import dataclass, field
import json
from pathlib import Path


@dataclass
class Account:
    """One signed-in Claude account.

    `token` is held only long enough to probe quota and is never serialised.
    """

    alias: str
    email: str | None = None
    org: str | None = None
    org_uuid: str | None = None
    plan: str | None = None
    is_active: bool = False
    access_expires_at: int | None = None
    refresh_expires_at: int | None = None
    token: str | None = field(default=None, repr=False)

    rate_limit_tier: str | None = None

    @property
    def plan_label(self) -> str:
        name = {"max": "Max", "team": "Team", "pro": "Pro", "free": "Free"}.get(self.plan, self.plan or "Unknown plan")
        tier = self.rate_limit_tier or ""
        if tier.endswith("_max_20x"):
            return name + " 20×"
        if tier.endswith("_max_5x"):
            return name + " 5×"
        return name

    def public(self) -> dict:
        """The view that may leave this process. Never includes a token."""
        return {
            "alias": self.alias,
            "email": self.email,
            "org": self.org,
            "plan": self.plan,
            "plan_label": self.plan_label,
            "rate_limit_tier": self.rate_limit_tier,
            "is_active": self.is_active,
            "access_expires_at": self.access_expires_at,
            "refresh_expires_at": self.refresh_expires_at,
        }


class Backend:
    """A source of accounts for one platform."""

    name = "base"
    #: Whether this platform exposes more than the one signed-in account.
    has_profiles = False
    #: Whether changing the machine-wide default account is supported here.
    can_switch = False

    def accounts(self) -> list[Account]:
        raise NotImplementedError

    def capabilities(self) -> dict:
        return {
            "backend": self.name,
            "profiles": self.has_profiles,
            "switch": self.can_switch,
        }


def read_json(path: Path) -> dict | None:
    """Read a JSON file, returning None rather than raising on damaged input."""
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        return None
    except (OSError, ValueError) as exc:
        raise BackendError(f"could not read {path.name}: {exc}") from exc


class BackendError(RuntimeError):
    """Credentials could not be located or decoded on this platform."""
