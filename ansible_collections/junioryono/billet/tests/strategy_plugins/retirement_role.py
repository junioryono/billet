"""Test-only external adapters around unchanged main.yml, including handlers.

Pinned with the suite's ansible-core version. The executor resolves and templates
real tasks before these adapters run; no condition, include or fact is replaced.
"""
import importlib.util
import os

from ansible.executor.task_executor import TaskExecutor
from ansible.plugins.strategy.linear import StrategyModule as Linear


class StrategyModule(Linear):
    def run(self, iterator, play_context):
        spec = importlib.util.spec_from_file_location('retirement_role_host', os.environ['BILLET_GATE_ROLE_HELPER'])
        host = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(host)
        self._role_host = host
        original = TaskExecutor._get_action_handler_with_module_context

        def resolve(executor, templar):
            handler, context = original(executor, templar)
            run = handler.run

            def observed_run(tmp=None, task_vars=None):
                return host.action(handler, run, tmp, task_vars)

            handler.run = observed_run
            return handler, context

        TaskExecutor._get_action_handler_with_module_context = resolve
        try:
            return super().run(iterator, play_context)
        finally:
            TaskExecutor._get_action_handler_with_module_context = original

    def _execute_meta(self, task, play_context, iterator, target_host):
        result = super()._execute_meta(task, play_context, iterator, target_host)
        state = iterator.get_state_for_host(target_host.name)
        self._role_host.log('meta.jsonl', dict(
            pass_name=self._role_host.settings()['pass'], task=task.get_name().split(' : ', 1)[-1],
            action=task._get_meta(), run_state=state.run_state.name,
            pending=list(state.handler_notifications)))
        return result
