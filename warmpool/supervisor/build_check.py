"""
Build-time check for Dockerfile.warmpool-launcher, run against the image's
real vLLM. The unit tests stub vLLM, so they cannot see what this catches: a
vLLM module the launcher imports that has moved (the import below fails), a
vLLM call that no longer binds the way the launcher makes it, an engine-config
attribute the follower branch reads, and the sleep routes and drain mode the
scaling manager calls.
Not shipped: the Dockerfile bind-mounts it for the one RUN that runs it.
Run as: python3 build_check.py <expected vLLM version>
"""

import inspect
import sys
import typing

import gputranslator  # noqa: F401
import launcher
import psutil
import uvicorn  # noqa: F401
import vllm

# Every vLLM call the launcher makes, in the shape it makes it: the same
# positional and keyword arguments, bound to the real signature. A parameter
# that is gone or renamed, or a new required one, fails the build. (Two
# positional parameters swapped under the same names are not caught.)
_X = object()
CALLS = [
    ("make_arg_parser", launcher.make_arg_parser, (_X,), {}),
    ("validate_parsed_serve_args", launcher.validate_parsed_serve_args, (_X,), {}),
    ("cli_env_setup", launcher.cli_env_setup, (), {}),
    ("run_server", launcher.run_server, (_X,), {}),
    ("AsyncEngineArgs.from_cli_args", launcher.AsyncEngineArgs.from_cli_args, (_X,), {}),
    (
        "AsyncEngineArgs.create_engine_config",
        launcher.AsyncEngineArgs.create_engine_config,
        (_X,),
        {"usage_context": _X, "headless": True},
    ),
    (
        "propagate_flash_late_interaction",
        launcher.propagate_flash_late_interaction,
        (_X, _X),
        {},
    ),
    (
        "MultiprocExecutor.__init__",
        launcher.MultiprocExecutor.__init__,
        (_X, _X),
        {"monitor_workers": False},
    ),
    (
        "MultiprocExecutor.start_worker_monitor",
        launcher.MultiprocExecutor.start_worker_monitor,
        (_X,),
        {"inline": True},
    ),
]

# What the follower branch reads from the engine config.
PARALLEL_CONFIG_ATTRS = ["node_rank_within_dp", "master_addr", "master_port"]

# What the scaling manager calls on every warm engine (internal/warmpool/pool):
# the dev routes, and the drain's mode=wait.
SLEEP_ROUTES = {"/sleep", "/wake_up", "/is_sleeping"}


def _vllm_api_problems() -> list[str]:
    bad = []
    from vllm.config import ParallelConfig

    fields = set(getattr(ParallelConfig, "__dataclass_fields__", {}))
    fields |= set(getattr(ParallelConfig, "model_fields", {}) or {})
    for name in PARALLEL_CONFIG_ATTRS:
        if name not in fields and not hasattr(ParallelConfig, name):
            bad.append(f"ParallelConfig has no {name}")
    from vllm.v1.engine import PauseMode

    if "wait" not in typing.get_args(PauseMode):
        bad.append(f"PauseMode {typing.get_args(PauseMode)} has no 'wait'")
    from vllm.entrypoints.serve.dev.sleep import api_router

    paths = {getattr(r, "path", "") for r in api_router.router.routes}
    if not SLEEP_ROUTES <= paths:
        bad.append(f"sleep router lacks {sorted(SLEEP_ROUTES - paths)}")
    return bad


def main() -> int:
    want = sys.argv[1]
    if vllm.__version__ != want:
        print(f"vllm is {vllm.__version__}, want {want}")
        return 1
    bad = []
    for name, fn, args, kwargs in CALLS:
        try:
            inspect.signature(fn).bind(*args, **kwargs)
        except TypeError as err:
            bad.append(f"{name}{inspect.signature(fn)}: {err}")
    try:
        bad += _vllm_api_problems()
    except ImportError as err:
        bad.append(f"vLLM module moved: {err}")
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
