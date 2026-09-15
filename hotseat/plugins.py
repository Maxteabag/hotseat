"""Optional integrations discovered from installed Python entry points."""
from functools import lru_cache
from importlib.metadata import entry_points
import os
import warnings

API_VERSION=1
GROUP="hotseat.plugins"

@lru_cache(maxsize=1)
def installed():
    if os.environ.get("HOTSEAT_PLUGINS", "").lower() in ("none","off","0"):
        return ()
    found=[]
    for entry in sorted(entry_points(group=GROUP),key=lambda e:e.name):
        try:
            plugin=entry.load()()
            if getattr(plugin,"api_version",None)!=API_VERSION:
                raise ValueError("incompatible plugin API version")
            if any(name==entry.name for name,_ in found):
                raise ValueError("duplicate plugin name")
            found.append((entry.name,plugin))
        except Exception as exc:
            warnings.warn(f"Hotseat plugin {entry.name} could not load: {exc}",RuntimeWarning)
    return tuple(found)


def configure_cli(add,nodes):
    for _,plugin in installed():
        hook=getattr(plugin,"register_commands",None)
        if hook:hook(add,nodes)


def snapshot(accounts):
    out={}
    for name,plugin in installed():
        hook=getattr(plugin,"snapshot",None)
        if hook:
            try:out[name]=hook(accounts)
            except Exception as exc:out[name]={"error":str(exc)}
    return out


def inspect_detail(identifier):
    for _,plugin in installed():
        hook=getattr(plugin,"inspect",None)
        if hook:
            try:result=hook(identifier)
            except Exception as exc:
                from .inspect import InspectError
                raise InspectError(str(exc)) from exc
            if result is not None:return result
    return None


def dashboard_scripts():
    scripts=[]
    for _,plugin in installed():
        hook=getattr(plugin,"dashboard_script",None)
        if hook:
            try:scripts.append("<script>"+hook()+"</script>")
            except Exception as exc:warnings.warn(f"Hotseat plugin dashboard failed: {exc}",RuntimeWarning)
    return "\n".join(scripts)
