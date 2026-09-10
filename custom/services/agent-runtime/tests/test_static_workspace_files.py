"""Run on Linux: actual permissions, symlinks, interrupted cleanup and locking."""
import hashlib
import multiprocessing
import os
import sys

import pytest

if sys.platform != 'linux':
    pytest.skip('Linux filesystem semantics required', allow_module_level=True)

from app.static_workspace_files import manage


def key(run): return hashlib.sha256(run.encode()).hexdigest()[:32]


def operate(root, action, run): manage(str(root), action, run, key(run), 'claim-1')


@pytest.fixture
def storage(tmp_path):
    if os.geteuid() != 0: pytest.skip('Run in the Linux test container as root')
    operate(tmp_path, 'prepare', 'a')
    operate(tmp_path, 'prepare', 'b')
    return tmp_path


def test_recovery_preserves_edits_and_receipts(storage):
    work = storage/'workspaces'/key('a')/'same.txt'
    receipt = storage/'receipts'/key('a')/'result.json'
    work.write_bytes(b'edited bytes'); receipt.write_bytes(b'execution receipt')
    operate(storage, 'prepare', 'a')
    assert work.read_bytes() == b'edited bytes'
    assert receipt.read_bytes() == b'execution receipt'
    assert work.parent.stat().st_uid == 1000
    assert receipt.parent.stat().st_uid == 0
    assert receipt.parent.stat().st_mode & 0o777 == 0o700


def test_cleanup_does_not_follow_links_or_touch_siblings(storage):
    a, b = (storage/'workspaces'/key(run) for run in ('a','b'))
    (b/'same.txt').write_bytes(b'keep')
    (a/'same.txt').write_bytes(b'delete')
    (a/'sibling').symlink_to(b, target_is_directory=True)
    (a/'outside').symlink_to('/etc', target_is_directory=True)
    operate(storage, 'clean', 'a')
    operate(storage, 'clean', 'a')
    assert not a.exists() and (b/'same.txt').read_bytes() == b'keep'
    with pytest.raises(ValueError): operate(storage, 'prepare', 'a')


def test_substituted_root_is_rejected(storage):
    a, b = (storage/'workspaces'/key(run) for run in ('a','b'))
    (b/'keep').write_text('keep')
    a.rmdir(); a.symlink_to(b, target_is_directory=True)
    with pytest.raises(OSError): operate(storage, 'clean', 'a')
    assert (b/'keep').read_text() == 'keep'


def test_interrupted_cleanup_retries_without_reinitialization(storage, monkeypatch):
    import app.static_workspace_files as fs
    original = fs.shutil.rmtree
    def fail(*args, **kwargs): raise OSError('interrupted')
    fail.avoids_symlink_attacks = True
    monkeypatch.setattr(fs.shutil, 'rmtree', fail)
    with pytest.raises(OSError): operate(storage, 'clean', 'a')
    with pytest.raises(ValueError): operate(storage, 'prepare', 'a')
    monkeypatch.setattr(fs.shutil, 'rmtree', original)
    operate(storage, 'clean', 'a')
    assert not (storage/'workspaces'/key('a')).exists()


def test_parallel_cleanup_is_idempotent(storage):
    processes = [multiprocessing.Process(target=operate, args=(storage,'clean','a')) for _ in range(4)]
    for process in processes: process.start()
    for process in processes:
        process.join(10)
        assert process.exitcode == 0
    assert (storage/'workspaces'/key('b')).is_dir()


def test_wrong_claim_and_path_rejected(storage):
    with pytest.raises(ValueError): manage(str(storage),'clean','a',key('a'),'other-claim')
    with pytest.raises(ValueError): manage(str(storage),'clean','a','../b','claim-1')
    assert (storage/'workspaces'/key('a')).exists()
