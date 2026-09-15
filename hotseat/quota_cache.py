"""Cache Claude quota reads and retain dated results while the API backs off."""
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
from . import usage

MIN_INTERVAL = 60

def root():
    return Path(os.environ.get("XDG_CACHE_HOME", Path.home()/".cache"))/"hotseat"/"quota-cooldowns"


def store(path, data):
    try:
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd, temporary = tempfile.mkstemp(dir=path.parent)
        try:
            with os.fdopen(fd, "w") as out: json.dump(data, out)
            os.replace(temporary, path)
        finally:
            if os.path.exists(temporary): os.unlink(temporary)
    except OSError:
        pass


def cached(cache, now):
    warning = ""
    if cache.get("failures") or not isinstance(cache.get("data"), dict):
        warning = f"Usage API throttled (HTTP 429); retry in {max(1,int(cache['until']-now)+1)}s"
    if not isinstance(cache.get("data"), dict):
        raise usage.UsageError(warning)
    return {**cache['data'], '_checked_at': cache['checked_at'], '_cached': True, '_warning': warning}


def read(token):
    path = root()/(hashlib.sha256(token.encode()).hexdigest()+".json")
    now = time.time()
    try:
        cache = json.loads(path.read_text())
        if not isinstance(cache, dict): cache = {}
    except (OSError, ValueError): cache = {}
    until = cache.get('until', 0)
    if isinstance(until, (int, float)) and until > now:
        return cached(cache, now)
    try:
        result = usage.for_token(token)
        checked = time.time()
        store(path, {'data': result, 'checked_at': checked, 'until': checked+MIN_INTERVAL, 'failures': 0})
        return {**result, '_checked_at': checked, '_cached': False, '_warning': ''}
    except usage.UsageError as exc:
        if '429' in str(exc):
            failures = min(int(cache.get('failures', 0))+1, 5)
            delay = max(MIN_INTERVAL * 2**(failures-1), getattr(exc, 'retry_after', None) or 0)
            cache.update(until=now+delay, failures=failures)
            store(path, cache)
            return cached(cache, now)
        # An authentication rejection must never be masked by a cached success.
        raise
