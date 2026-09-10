"""Static claim validation and crash-recoverable per-run cleanup bookkeeping."""
import hashlib
import os
from pathlib import Path
from types import SimpleNamespace

from kubernetes_asyncio.client.exceptions import ApiException

LABEL = 'app.kubernetes.io/managed-by=weknora-agent-runtime'
ROLE = 'weknora.agent.storage-role'
PROGRAM = Path(__file__).with_name('static_workspace_files.py').read_text(encoding='utf-8')


def claim_name():
    name = os.environ.get('AGENT_WORKSPACE_PVC_NAME', '').strip()
    if not name:
        raise ValueError('AGENT_WORKSPACE_PVC_NAME must name an existing static claim')
    return name


def command(action, run_id, key, uid):
    return ['python3', '-c', PROGRAM, action, run_id, key, uid]


async def bound_claim(api, namespace):
    claim = await api.read_namespaced_persistent_volume_claim(claim_name(), namespace)
    expected = os.environ.get('AGENT_WORKSPACE_PV_NAME', '').strip()
    if (claim.status.phase != 'Bound' or not claim.spec.volume_name
            or (expected and claim.spec.volume_name != expected)
            or claim.metadata.deletion_timestamp
            or claim.spec.volume_mode not in (None, 'Filesystem')):
        raise RuntimeError('Workspace claim is not bound to the expected filesystem PV')
    return claim


async def track(api, namespace, payload, key, claim):
    name = 'agent-storage-' + key
    data = {'run_id': payload.run_id, 'key': key, 'claim': claim_name(), 'claim_uid': claim.metadata.uid}
    body = {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': name,
            'labels': {'app.kubernetes.io/managed-by': 'weknora-agent-runtime', ROLE: 'record'}}, 'data': data}
    try:
        await api.create_namespaced_config_map(namespace, body)
    except ApiException as exc:
        if exc.status != 409:
            raise
        old = await api.read_namespaced_config_map(name, namespace)
        if old.data != data or old.metadata.deletion_timestamp:
            raise RuntimeError('Workspace storage record ownership mismatch')


async def sweep_static(api, namespace, terminal):
    # Records are created BEFORE Pods, and outlive Pod deletion and worker crashes.
    from .kubernetes_workspace import PodContainer, pod_spec
    records = await api.list_namespaced_config_map(namespace, label_selector=LABEL+','+ROLE+'=record')
    for record in records.items:
        data = record.data or {}
        run, key = data.get('run_id'), data.get('key')
        if not run or hashlib.sha256(run.encode()).hexdigest()[:32] != key:
            raise RuntimeError('Invalid workspace storage record')
        if record.metadata.name != 'agent-storage-' + key or data.get('claim') != claim_name():
            raise RuntimeError('Unexpected workspace storage record')
        if not await terminal({'weknora.agent.run': run}):
            continue
        claim = await bound_claim(api, namespace)
        if data.get('claim_uid') != claim.metadata.uid:
            raise RuntimeError('Workspace PVC was replaced; refusing cleanup')
        name = 'agent-workspace-' + key
        try:
            pod = await api.read_namespaced_pod(name, namespace)
        except ApiException as exc:
            if exc.status != 404:
                raise
            pod = None
        if pod:
            labels = pod.metadata.labels or {}
            if labels.get('weknora.agent.run') != run or labels.get('weknora.agent.workspace') != key:
                raise RuntimeError('Workspace Pod ownership mismatch')
            if labels.get(ROLE) == 'cleanup':
                if pod.status.phase == 'Succeeded':
                    await PodContainer(api, namespace, name, pod.metadata.uid).delete()
                    await api.delete_namespaced_config_map(record.metadata.name, namespace,
                        body={'preconditions': {'uid': record.metadata.uid}})
                    continue
                if pod.status.phase != 'Failed':
                    continue
            await PodContainer(api, namespace, name, pod.metadata.uid).delete()
        spec = pod_spec(SimpleNamespace(run_id=run, owner_epoch=0), key, claim.metadata.uid)
        spec['metadata']['labels'][ROLE] = 'cleanup'
        spec['spec']['initContainers'] = []
        worker = spec['spec']['containers'][0]
        worker['workingDir'] = '/'
        worker['env'] = []
        worker['command'] = command('clean', run, key, claim.metadata.uid)
        worker['volumeMounts'] = [{'name': 'runtime-storage', 'mountPath': '/runtime-root'}]
        worker['securityContext']['capabilities']['add'] = ['DAC_OVERRIDE', 'FOWNER']
        try:
            await api.create_namespaced_pod(namespace, spec)
        except ApiException as exc:
            if exc.status != 409:
                raise
