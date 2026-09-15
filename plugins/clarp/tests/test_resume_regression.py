import io,json,unittest
from contextlib import redirect_stdout
from unittest.mock import patch
from hotseat_clarp import resume,Plugin
from hotseat import cli,plugins
from hotseat.collect import Collector
class ResumeJsonRegressionTest(unittest.TestCase):
    def test_json_go_runs_and_reports_success_or_failure(self):
        item = {"kind": "clarp", "id": "fixture", "name": "Fixture",
                "backend": "codex", "cause": "usage_limit"}
        for failure in (False, True):
            with self.subTest(failure=failure), patch.object(plugins,"installed",return_value=(("clarp",Plugin()),)), \
                    patch.object(resume, "stopped", return_value=[dict(item)]), \
                    patch.object(Collector, "build", return_value={"accounts": []}), \
                    patch.object(resume, "continue_item", return_value={
                        "id": "fixture", "continued": True},
                        side_effect=resume.ResumeError("fixture failure") if failure else None) as go, \
                    redirect_stdout(io.StringIO()) as output:
                code = cli.main(["resume", "--go", "--json"])
                go.assert_called_once()
                result = json.loads(output.getvalue())["results"][0]
                self.assertEqual(code, int(failure))
                self.assertEqual(result["continued"], not failure)
