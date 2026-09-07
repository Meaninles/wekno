"""Release orphan workspaces only after the authoritative run is terminal."""
import asyncio
import json
import logging
import os

import httpx

LABEL = "app.kubernetes.io/managed-by=weknora-agent-runtime"


async def sweep(client):
    async def terminal(labels):
        run = (labels or {}).get("weknora.agent.run")
        if not run:
            return False
        response = await client.post(os.environ["AGENT_RUNTIME_CONTROL_URL"].rstrip("/")+"/runs/status",
            headers={"Authorization":"Bearer "+os.environ["AGENT_RUNTIME_API_KEY"]},json={"run_id":run})
        response.raise_for_status()
        return response.json().get("status") in ("completed","failed","cancelled","incomplete")
    if os.environ.get("AGENT_WORKSPACE_BACKEND","docker") == "kubernetes":
        from kubernetes_asyncio import client as k8s, config
        from .kubernetes_workspace import PodContainer
        config.load_incluster_config()
        async with k8s.ApiClient() as transport:
            api=k8s.CoreV1Api(transport)
            ns=os.environ["AGENT_WORKSPACE_NAMESPACE"]
            for pod in (await api.list_namespaced_pod(ns,label_selector=LABEL)).items:
                if await terminal(pod.metadata.labels):
                    await PodContainer(api,ns,pod.metadata.name,pod.metadata.uid).delete()
            for volume in (await api.list_namespaced_persistent_volume_claim(ns,label_selector=LABEL)).items:
                if await terminal(volume.metadata.labels):
                    await api.delete_namespaced_persistent_volume_claim(volume.metadata.name,ns,
                        body={"preconditions":{"uid":volume.metadata.uid}})
    else:
        import aiodocker
        async with aiodocker.Docker() as docker:
            filters=json.dumps({"label":[LABEL]})
            for container in await docker.containers.list(all=True,filters=filters):
                if await terminal((await container.show())["Config"]["Labels"]):
                    await container.delete(force=True)
            volumes=await docker.volumes.list(filters={"label":[LABEL]})
            for info in volumes.get("Volumes") or []:
                if await terminal(info.get("Labels")):
                    await (await docker.volumes.get(info["Name"])).delete()


async def housekeeping():
    async with httpx.AsyncClient(timeout=15) as client:
        while True:
            try:
                await sweep(client)
            except Exception as exc:
                logging.getLogger("agent-runtime").warning("workspace_cleanup_failed=%s",type(exc).__name__)
            await asyncio.sleep(60)
