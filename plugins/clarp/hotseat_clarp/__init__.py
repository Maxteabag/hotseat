"""Clarp integration. Loaded only when hotseat-clarp is installed."""
from importlib.resources import files

class Plugin:
    api_version=1

    def register_commands(self,add,nodes):
        from . import cli
        node=add("clarp",cli.cmd_clarp,"Clarp agents and account usage")
        node.add_argument("--live",action="store_true")
        node.add_argument("--backend")
        nodes["resume"].set_defaults(handler=cli.cmd_resume)
        nodes["inspect"].set_defaults(handler=cli.cmd_inspect)

    def snapshot(self,accounts):
        from . import clarp,resume
        default=next((a["alias"] for a in accounts if a.get("is_active")),None)
        try:overview=clarp.overview(default_alias=default)
        except clarp.ClarpError:overview=None
        items=[i for i in resume.stopped() if i["kind"]=="clarp"]
        for item in items:item["readiness"]=resume.readiness(item,accounts,default)
        return {"agents":overview,"stopped":items}

    def inspect(self,identifier):
        from .inspect import _clarp_detail
        return _clarp_detail(identifier)

    def dashboard_script(self):
        return files("hotseat_clarp").joinpath("dashboard.js").read_text()
