import os

from ansible.plugins.callback import CallbackBase


class CallbackModule(CallbackBase):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = "aggregate"
    CALLBACK_NAME = "billet_kill_after"
    CALLBACK_NEEDS_ENABLED = True

    def v2_runner_on_ok(self, result):
        target = os.environ.get("BILLET_KILL_AFTER_TASK", "")
        if target and result._task.get_name().endswith(target):
            os._exit(99)
