"""Opt-in Docker Linux bind-boundary check; does not certify Kubernetes/CCE."""
import hashlib
import os
import uuid

import aiodocker
import pytest

from app.static_workspace import PROGRAM

pytestmark = pytest.mark.skipif(os.environ.get('STATIC_WORKSPACE_MOUNT_TEST') != '1', reason='Opt-in real Docker mounts')


async def test_shared_volume_subpaths_preserve_files_and_isolate_deletion():
    image = os.environ.get('AGENT_WORKSPACE_IMAGE', 'weknora-agent-workspace:local')
    name = 'weknora-static-test-' + uuid.uuid4().hex
    keys = {run:hashlib.sha256(run.encode()).hexdigest()[:32] for run in ('a','b')}
    async with aiodocker.Docker() as docker:
        volume = await docker.volumes.create({'Name': name, 'Labels': {'weknora.test':'static-subpaths'}})
        async def run(command, mounts, caps):
            container = await docker.containers.create(config={'Image':image, 'Cmd':command,
                'HostConfig':{'NetworkMode':'none', 'ReadonlyRootfs':True, 'CapDrop':['ALL'],
                            'CapAdd':caps, 'SecurityOpt':['no-new-privileges:true'], 'Mounts':mounts}})
            try:
                await container.start()
                result = await container.wait(timeout=60)
                output = await container.log(stdout=True, stderr=True)
                assert result['StatusCode'] == 0, output
            finally:
                await container.delete(force=True)
        root = [{'Type':'volume','Source':name,'Target':'/runtime-root'}]
        async def manage(action, run_id):
            await run(['python3','-c',PROGRAM,action,run_id,keys[run_id],'test-claim'], root,
                      ['CHOWN','FOWNER','DAC_OVERRIDE'])
        def mounts(run_id):
            return [{'Type':'volume','Source':name,'Target':target,
                     'VolumeOptions':{'Subpath':prefix+'/'+keys[run_id]}}
                    for prefix,target in [('workspaces','/workspace'),('receipts','/control')]]
        user = 'import os; os.setgroups([]); os.setgid(1000); os.setuid(1000); '
        try:
            for run_id in ('a','b'): await manage('prepare',run_id)
            await run(['python3','-c',user+"from pathlib import Path; Path('/workspace/same.txt').write_text('B'); assert not os.access('/control',os.R_OK)"], mounts('b'), ['SETUID','SETGID'])
            await run(['python3','-c',user+"from pathlib import Path; Path('/workspace/same.txt').write_text('A'); assert not Path('/runtime-root').exists(); Path('/workspace/outside').symlink_to('/etc'); Path('/workspace/same.txt').unlink()"], mounts('a'), ['SETUID','SETGID'])
            await manage('prepare','b')
            await manage('clean','a')
            await run(['python3','-c',user+"from pathlib import Path; assert Path('/workspace/same.txt').read_text() == 'B'"], mounts('b'), ['SETUID','SETGID'])
        finally:
            await volume.delete()
