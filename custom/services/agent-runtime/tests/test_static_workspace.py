import asyncio
import hashlib
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import pytest
from kubernetes_asyncio.client.exceptions import ApiException

from app.kubernetes_workspace import KubernetesBackend, pod_spec
from app.static_workspace import ROLE, bound_claim, sweep_static, track
from test_run import request


def identity(run):
    return hashlib.sha256(run.encode()).hexdigest()[:32]


@pytest.fixture(autouse=True)
def settings(monkeypatch):
    monkeypatch.setenv('AGENT_WORKSPACE_PVC_NAME', 'shared')
    monkeypatch.setenv('AGENT_WORKSPACE_PV_NAME', 'static-pv')
    monkeypatch.setenv('AGENT_WORKSPACE_NAMESPACE', 'workspaces')
    monkeypatch.setenv('AGENT_WORKSPACE_IMAGE', 'workspace:test')


def claim():
    return NS(metadata=NS(uid='claim-1', deletion_timestamp=None), status=NS(phase='Bound'),
              spec=NS(volume_name='static-pv', volume_mode='Filesystem'))


def test_subpaths_preserve_recovery_identity_and_isolate_runs():
    a, b = request().model_copy(update={'run_id':'a'}), request().model_copy(update={'run_id':'b'})
    before = pod_spec(a, identity('a'), 'claim-1')
    a.owner_epoch += 1
    after = pod_spec(a, identity('a'), 'claim-1')
    other = pod_spec(b, identity('b'), 'claim-1')
    mounts = lambda p: p['spec']['containers'][0]['volumeMounts']
    assert mounts(before) == mounts(after)
    assert {m['subPath'] for m in mounts(before) if 'subPath' in m}.isdisjoint(
        m['subPath'] for m in mounts(other) if 'subPath' in m)
    assert before['spec']['volumes'][0]['persistentVolumeClaim']['claimName'] == 'shared'
    assert len([v for v in before['spec']['volumes'] if 'persistentVolumeClaim' in v]) == 1
    assert all(m['mountPath'] != '/runtime-root' for m in mounts(before))
    assert before['spec']['automountServiceAccountToken'] is False
    with pytest.raises(ValueError): pod_spec(a, '../b', 'claim-1')


@pytest.mark.parametrize('fault', ['pending', 'wrong_pv', 'deleting', 'block'])
async def test_claim_preflight_fails_closed(fault):
    c = claim()
    if fault == 'pending': c.status.phase = 'Pending'
    if fault == 'wrong_pv': c.spec.volume_name = 'database'
    if fault == 'deleting': c.metadata.deletion_timestamp = 'now'
    if fault == 'block': c.spec.volume_mode = 'Block'
    api = NS(read_namespaced_persistent_volume_claim=AsyncMock(return_value=c))
    with pytest.raises(RuntimeError): await bound_claim(api, 'workspaces')


async def test_record_rejects_recreated_claim():
    api = NS(create_namespaced_config_map=AsyncMock(side_effect=ApiException(status=409)),
        read_namespaced_config_map=AsyncMock(return_value=NS(data={'claim_uid': 'old'})))
    with pytest.raises(RuntimeError): await track(api, 'workspaces', request().model_copy(update={'run_id':'a'}), identity('a'), claim())


def sweep_api():
    run, key = 'a', identity('a')
    record = NS(metadata=NS(name='agent-storage-'+key, uid='record-1'),
                data={'run_id':run, 'key':key, 'claim':'shared', 'claim_uid':'claim-1'})
    return NS(list_namespaced_config_map=AsyncMock(return_value=NS(items=[record])),
              read_namespaced_persistent_volume_claim=AsyncMock(return_value=claim()),
              read_namespaced_pod=AsyncMock(side_effect=ApiException(status=404)),
              create_namespaced_pod=AsyncMock(), delete_namespaced_config_map=AsyncMock())


async def test_nonterminal_never_touches_pod_or_storage():
    api = sweep_api()
    await sweep_static(api, 'workspaces', AsyncMock(return_value=False))
    api.read_namespaced_persistent_volume_claim.assert_not_called()
    api.create_namespaced_pod.assert_not_called()


async def test_orphan_record_schedules_idempotent_cleanup_without_pvc_mutations():
    api = sweep_api()
    api.create_namespaced_pod.side_effect = ApiException(status=409)
    await sweep_static(api, 'workspaces', AsyncMock(return_value=True))
    spec = api.create_namespaced_pod.call_args.args[1]
    assert spec['metadata']['labels'][ROLE] == 'cleanup'
    assert spec['spec']['containers'][0]['command'][-4:] == ['clean', 'a', identity('a'), 'claim-1']
    assert spec['spec']['initContainers'] == []
    api.delete_namespaced_config_map.assert_not_called()


async def test_cleanup_success_releases_record_only_after_pod_deletion(monkeypatch):
    api = sweep_api()
    api.read_namespaced_pod.side_effect = None
    api.read_namespaced_pod.return_value = NS(metadata=NS(uid='pod-1', labels={ROLE:'cleanup',
        'weknora.agent.run':'a', 'weknora.agent.workspace':identity('a')}), status=NS(phase='Succeeded'))
    deleted = []
    async def delete(self): deleted.append(self.uid)
    monkeypatch.setattr('app.kubernetes_workspace.PodContainer.delete', delete)
    await sweep_static(api, 'workspaces', AsyncMock(return_value=True))
    assert deleted == ['pod-1']
    assert api.delete_namespaced_config_map.call_args.kwargs['body'] == {'preconditions': {'uid':'record-1'}}
    api.create_namespaced_pod.assert_not_called()


async def test_close_only_removes_execution_boundary_for_finalization_reopen():
    backend = KubernetesBackend(request(), AsyncMock())
    backend.container = NS(delete=AsyncMock())
    backend.api_client = NS(close=AsyncMock())
    backend.api = NS()  # deliberately exposes NO PVC create/delete operations
    await backend.close()
    assert backend.container is None and backend.api is None
    backend.control.post.assert_not_called()


async def test_backend_initializes_and_reopens_only_existing_claim(monkeypatch):
    from app import kubernetes_workspace as module
    api = NS(read_namespaced_persistent_volume_claim=AsyncMock(return_value=claim()),
             create_namespaced_config_map=AsyncMock(), create_namespaced_pod=AsyncMock(),
             read_namespaced_pod=AsyncMock())
    ready = NS(metadata=NS(uid='pod-1'), status=NS(phase='Running',container_statuses=[NS(ready=True)]))
    api.create_namespaced_pod.return_value = ready
    api.read_namespaced_pod.side_effect = [ApiException(status=404), ready, ApiException(status=404), ready]
    transport = NS(close=AsyncMock())
    monkeypatch.setattr(module.config,'load_incluster_config',lambda:None)
    monkeypatch.setattr(module.client,'ApiClient',lambda:transport)
    monkeypatch.setattr(module.client,'CoreV1Api',lambda _:api)
    monkeypatch.setattr(module.PodContainer,'delete',AsyncMock())
    backend = KubernetesBackend(request(),NS(post=AsyncMock()))
    await backend.initialize()
    first = api.create_namespaced_pod.call_args.args[1]
    await backend.close()
    await backend.initialize()
    second = api.create_namespaced_pod.call_args.args[1]
    assert first['spec']['containers'][0]['volumeMounts'] == second['spec']['containers'][0]['volumeMounts']
    assert api.create_namespaced_config_map.call_count == 2
    await backend.close()


async def test_failed_preflight_creates_no_pod_or_record(monkeypatch):
    from app import kubernetes_workspace as module
    api = NS(read_namespaced_persistent_volume_claim=AsyncMock(side_effect=ApiException(status=404)),
             create_namespaced_config_map=AsyncMock(),create_namespaced_pod=AsyncMock())
    monkeypatch.setattr(module.config,'load_incluster_config',lambda:None)
    monkeypatch.setattr(module.client,'ApiClient',lambda:NS(close=AsyncMock()))
    monkeypatch.setattr(module.client,'CoreV1Api',lambda _:api)
    backend = KubernetesBackend(request(),NS(post=AsyncMock()))
    with pytest.raises(ApiException): await backend.initialize()
    api.create_namespaced_pod.assert_not_called()
    api.create_namespaced_config_map.assert_not_called()
    await backend.close()
