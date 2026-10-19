"""Linux inference launch boundary: discard console output and memory dumps.

Health, aggregate metrics and protocol usage remain available over HTTP.
"""
import json
import os
from pathlib import Path
import resource
import subprocess


def private_environment(base=None):
    env = dict(os.environ if base is None else base)
    for key in list(env):
        if key.startswith(("VLLM_DEBUG_DUMP", "VLLM_TORCH_PROFILER", "VLLM_PROFILER",
                           "TORCH_TRACE", "TORCH_LOGS", "CUDA_COREDUMP")):
            env.pop(key)
    env.pop("PYTHONFAULTHANDLER", None)
    env.update({"VLLM_SERVER_DEV_MODE": "0", "VLLM_LOGGING_LEVEL": "CRITICAL",
                "VLLM_TRACE_FUNCTION": "0",
                "HF_HUB_DISABLE_TELEMETRY": "1", "VLLM_NO_USAGE_STATS": "1",
                "CUDA_ENABLE_COREDUMP_ON_EXCEPTION": "0",
                "CUDA_ENABLE_CPU_COREDUMP_ON_EXCEPTION": "0",
                "TORCH_SHOW_CPP_STACKTRACES": "0"})
    return env


def start_private(argv, *, cwd, pid_file, env=None):
    """Launch detached; the caller must drain before replacing a live PID."""
    pid_file = Path(pid_file)
    if pid_file.exists():
        pid = int(pid_file.read_text().strip())
        if Path(f"/proc/{pid}/cmdline").exists() and Path(f"/proc/{pid}/cmdline").read_bytes():
            raise RuntimeError("Inference process is already running; drain it first")
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    child_env = private_environment(env)
    config = Path(cwd) / "private-logging.json"
    config.write_text(json.dumps({"version": 1, "disable_existing_loggers": True,
        "handlers": {"discard": {"class": "logging.NullHandler"}},
        "root": {"handlers": ["discard"], "level": "CRITICAL"},
        "loggers": {name: {"handlers": ["discard"], "level": "CRITICAL", "propagate": False}
                    for name in ("vllm", "uvicorn", "uvicorn.error", "uvicorn.access")}}))
    config.chmod(0o600)
    child_env["VLLM_LOGGING_CONFIG_PATH"] = str(config)
    process = subprocess.Popen(argv, cwd=cwd, env=child_env, stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    pid_file.write_text(str(process.pid) + "\n")
    return process


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cwd", required=True)
    parser.add_argument("--pid-file", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("a command after -- is required")
    process = start_private(command, cwd=args.cwd, pid_file=args.pid_file)
    print(json.dumps({"pid": process.pid, "private_output": True}))
