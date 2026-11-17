"""
Build-time check for Dockerfile.warmpool-launcher, run against the image's
real vLLM. The unit tests stub vLLM, so they cannot see either failure this
catches: a vLLM module the launcher imports that has moved (the import below
fails), or a vLLM call whose parameters changed (the signature checks fail).
Not shipped: the Dockerfile deletes it after running it.
Run as: python3 build_check.py <expected vLLM version>
"""

import inspect
import sys

import gputranslator  # noqa: F401
import launcher
import psutil
import uvicorn  # noqa: F401
import vllm

# Every vLLM call the launcher makes, with the parameters it passes by name or
# position. A parameter that is gone or renamed fails the build.
CALLS = [
    ("make_arg_parser", launcher.make_arg_parser, ["parser"]),
    ("validate_parsed_serve_args", launcher.validate_parsed_serve_args, ["args"]),
    ("cli_env_setup", launcher.cli_env_setup, []),
    ("run_server", launcher.run_server, ["args"]),
    ("AsyncEngineArgs.from_cli_args", launcher.AsyncEngineArgs.from_cli_args, ["args"]),
    (
        "AsyncEngineArgs.create_engine_config",
        launcher.AsyncEngineArgs.create_engine_config,
        ["usage_context", "headless"],
    ),
    (
        "propagate_flash_late_interaction",
        launcher.propagate_flash_late_interaction,
        ["args", "engine_args"],
    ),
    (
        "MultiprocExecutor.__init__",
        launcher.MultiprocExecutor.__init__,
        ["vllm_config", "monitor_workers"],
    ),
    (
        "MultiprocExecutor.start_worker_monitor",
        launcher.MultiprocExecutor.start_worker_monitor,
        ["inline"],
    ),
]


def main() -> int:
    want = sys.argv[1]
    if vllm.__version__ != want:
        print(f"vllm is {vllm.__version__}, want {want}")
        return 1
    bad = []
    for name, fn, params in CALLS:
        have = inspect.signature(fn).parameters
        missing = [p for p in params if p not in have]
        if missing:
            bad.append(f"{name}{inspect.signature(fn)} lacks {missing}")
    if not hasattr(launcher.UsageContext, "OPENAI_API_SERVER"):
        bad.append("UsageContext has no OPENAI_API_SERVER")
    # psutil 7.2 waits through pidfd_open() and re-raises its EINVAL for a PID
    # being reaped (giampaolo/psutil#2715): a DELETE then fails with a 500.
    # The launcher's _wait_procs absorbs it; a re-vendored launcher without it
    # must not ship on such a psutil.
    ver = tuple(int(x) for x in psutil.__version__.split(".")[:2])
    if ver >= (7, 2) and not hasattr(launcher, "_wait_procs"):
        bad.append(f"psutil {psutil.__version__} raises EINVAL from wait; launcher has no _wait_procs")
    for line in bad:
        print("launcher check:", line)
    if bad:
        return 1
    print(
        f"vllm {vllm.__version__}, psutil {psutil.__version__}: launcher imports ok, "
        f"{len(CALLS)} call signatures ok"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
