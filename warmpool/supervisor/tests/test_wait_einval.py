"""
Tests for _wait_procs: psutil 7.2.x waits through pidfd_open() and re-raises
its EINVAL (giampaolo/psutil#2715), which the kernel returns for a PID whose
task is being reaped. A DELETE that escalated to SIGKILL then failed with
HTTP 500 "[Errno 22] Invalid argument". The pinned test psutil (7.1.3) never
uses pidfd, so psutil.wait_procs is mocked to raise it.
Run as:
python -m pytest tests/test_wait_einval.py -v
"""

import asyncio
import errno
import sys
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


def _proc(running):
    p = MagicMock()
    p.is_running.return_value = running
    return p


def _einval():
    return OSError(errno.EINVAL, "Invalid argument")


def test_await_procs_survives_einval_from_a_reaped_process():
    # The path a DELETE takes (stop_async -> _await_procs). Before the fix the
    # OSError escaped and the request failed with a 500.
    gone = _proc(running=False)
    with patch.object(launcher.psutil, "wait_procs", side_effect=_einval()):
        asyncio.run(launcher._await_procs({gone}, 1.0))


def test_einval_rechecks_and_keeps_waiting_for_the_survivors():
    gone, alive = _proc(running=False), _proc(running=True)
    wait = MagicMock(side_effect=[_einval(), ([], [alive])])
    with patch.object(launcher.psutil, "wait_procs", wait):
        left = launcher._wait_procs([gone, alive], 5)
    assert left == [alive]
    # The second wait is for the survivor only, not for the reaped process.
    assert wait.call_args_list[1].args[0] == [alive]


def test_einval_after_the_deadline_returns_what_is_still_running():
    alive = _proc(running=True)
    with patch.object(launcher.psutil, "wait_procs", side_effect=_einval()):
        assert launcher._wait_procs([alive], 0) == [alive]


def test_other_errors_still_raise():
    with patch.object(
        launcher.psutil, "wait_procs", side_effect=OSError(errno.EPERM, "no")
    ):
        with pytest.raises(OSError):
            launcher._wait_procs([_proc(running=True)], 1)
