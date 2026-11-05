"""Exercise real process output, exceptions and dump limits with synthetic data."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest

from service import request_diagnostics


class PrivacyTest(unittest.TestCase):
    def test_backend_extensions_cannot_enter_diagnostics(self):
        secret = "synthetic-private-prompt-image-tool-key"
        prepared = SimpleNamespace(prompt_tokens=12, images=[secret], started_at=0,
            timings={"first_answer_ms": 5.0, secret: secret},
            backend_usage={"prompt_tokens": 12, "completion_tokens": secret,
                "prompt_tokens_details": {"cached_tokens": 8, "token_ids": [41, 42]}, secret: secret},
            backend_metrics={"time_in_queue": 0.1, "prompt_token_ids": [41, 42], "output": secret})
        result = json.dumps(request_diagnostics(prepared, "resp_generated-id"))
        self.assertNotIn(secret, result)
        self.assertNotIn("token_ids", result)
        self.assertEqual(8, json.loads(result)["usage"]["prompt_tokens_details"]["cached_tokens"])

    def test_native_stdout_exception_and_child_limits(self):
        with tempfile.TemporaryDirectory() as directory:
            module_dir = str(Path(__file__).resolve().parent)
            child = """import os,resource,sys
assert resource.getrlimit(resource.RLIMIT_CORE)==(0,0)
assert os.readlink('/proc/self/fd/1')=='/dev/null'
assert os.readlink('/proc/self/fd/2')=='/dev/null'
assert 'VLLM_DEBUG_DUMP_PATH' not in os.environ
assert os.environ['CUDA_ENABLE_COREDUMP_ON_EXCEPTION']=='0'
os.write(1,b'synthetic-request-body')
os.write(2,b'synthetic-output-token-ids')
raise ValueError('synthetic-api-key')
"""
            runner = f"""import sys
sys.path.insert(0,{module_dir!r})
from private_runtime import start_private
p=start_private([sys.executable,'-c',{child!r}],cwd={directory!r},
    pid_file={str(Path(directory)/'test.pid')!r})
assert p.wait(timeout=10)==1
"""
            env = dict(os.environ, VLLM_DEBUG_DUMP_PATH=directory, CUDA_ENABLE_COREDUMP_ON_EXCEPTION="1")
            result = subprocess.run([sys.executable, "-c", runner], env=env, capture_output=True)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertEqual(b"", result.stdout)
            self.assertEqual(b"", result.stderr)
            self.assertEqual({"private-logging.json", "test.pid"}, {p.name for p in Path(directory).iterdir()})


if __name__ == "__main__":
    unittest.main()
