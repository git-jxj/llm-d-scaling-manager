"""
Tests for the follower-rank branch of vllm_kickoff, the one change this copy
carries on top of the upstream launcher (see the header of launcher.py).
Run as:
python -m pytest tests/test_follower_rank.py -v
"""

import io
import sys
from contextlib import ExitStack
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import pytest

# The same stubs as test_launcher.py; whichever test module loads first wins.
for _name in (
    "vllm",
    "vllm.utils",
    "vllm.utils.argparse_utils",
    "vllm.engine.arg_utils",
    "vllm.entrypoints.common.cli_args",
    "vllm.entrypoints.common.startup",
    "vllm.entrypoints.launchers.api_server.entry",
    "vllm.usage.usage_lib",
    "vllm.v1.executor.multiproc_executor",
):
    sys.modules.setdefault(_name, MagicMock())

import launcher  # noqa: E402
from launcher import VllmConfig, vllm_kickoff  # noqa: E402


def _kickoff(parsed_args, node_rank):
    """Run vllm_kickoff with vLLM and the fd plumbing replaced by mocks.

    Returns the mocks the assertions need.
    """
    parser = MagicMock()
    parser.parse_args.return_value = parsed_args
    engine_config = SimpleNamespace(
        parallel_config=SimpleNamespace(
            node_rank_within_dp=node_rank,
            master_addr="10.0.0.1",
            master_port=29500,
        )
    )
    engine_args = MagicMock()
    engine_args.create_engine_config.return_value = engine_config
    m = SimpleNamespace(
        executor_cls=MagicMock(),
        engine_args_cls=MagicMock(),
        run_server=MagicMock(),
        uvloop_run=MagicMock(),
        engine_config=engine_config,
    )
    m.engine_args_cls.from_cli_args.return_value = engine_args
    with ExitStack() as stack:
        stack.enter_context(patch.object(launcher.os, "setpgrp"))
        stack.enter_context(patch.object(launcher, "_close_inherited_sockets"))
        stack.enter_context(patch.object(launcher.os, "open", return_value=99))
        stack.enter_context(patch.object(launcher.os, "dup2"))
        stack.enter_context(patch.object(launcher.os, "close"))
        stack.enter_context(
            patch.object(launcher.os, "fdopen", side_effect=lambda *a: io.StringIO())
        )
        stack.enter_context(patch.object(sys, "stdout", MagicMock()))
        stack.enter_context(patch.object(sys, "stderr", MagicMock()))
        stack.enter_context(patch.object(launcher, "cli_env_setup"))
        stack.enter_context(
            patch.object(launcher, "FlexibleArgumentParser", return_value=parser)
        )
        stack.enter_context(
            patch.object(launcher, "make_arg_parser", side_effect=lambda p: p)
        )
        stack.enter_context(patch.object(launcher, "validate_parsed_serve_args"))
        stack.enter_context(
            patch.object(launcher, "AsyncEngineArgs", m.engine_args_cls)
        )
        stack.enter_context(
            patch.object(launcher, "MultiprocExecutor", m.executor_cls)
        )
        stack.enter_context(patch.object(launcher, "run_server", m.run_server))
        stack.enter_context(patch.object(launcher.uvloop, "run", m.uvloop_run))
        vllm_kickoff(
            VllmConfig(options="--model m --headless"), "/dev/null", False
        )
    return m


def test_follower_rank_runs_the_headless_executor_inline():
    m = _kickoff(SimpleNamespace(headless=True), node_rank=1)
    m.executor_cls.assert_called_once_with(m.engine_config, monitor_workers=False)
    m.executor_cls.return_value.start_worker_monitor.assert_called_once_with(
        inline=True
    )
    _, kwargs = m.engine_args_cls.from_cli_args.return_value.create_engine_config.call_args
    assert kwargs["headless"] is True
    m.run_server.assert_not_called()
    m.uvloop_run.assert_not_called()


@pytest.mark.parametrize(
    "parsed_args,node_rank",
    [
        (SimpleNamespace(headless=True), 0),  # the leader of a group
        (SimpleNamespace(headless=False), 0),  # an ordinary engine
        (SimpleNamespace(), 0),  # no --headless flag at all
    ],
)
def test_leader_and_single_pod_engines_run_the_api_server(parsed_args, node_rank):
    m = _kickoff(parsed_args, node_rank)
    m.executor_cls.assert_not_called()
    m.uvloop_run.assert_called_once()
    m.run_server.assert_called_once_with(parsed_args)
