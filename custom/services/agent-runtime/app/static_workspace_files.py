"""Trusted Linux-only directory lifecycle, executed in storage init/cleanup Pods.

Generated programs never receive this mount. All traversal uses directory FDs;
the retained tombstone fences delayed initializers after terminal cleanup.
"""
import contextlib
import fcntl
import hashlib
import json
import os
import re
import shutil
import sys


@contextlib.contextmanager
def directory(parent, name, create=False):
    if create:
        try:
            os.mkdir(name, 0o700, dir_fd=parent)
        except FileExistsError:
            pass
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
    try:
        yield fd
    finally:
        os.close(fd)


def save(fd, state):
    raw = json.dumps(state).encode()
    out = os.open('state.tmp', os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600, dir_fd=fd)
    with os.fdopen(out, 'wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    os.rename('state.tmp', 'state.json', src_dir_fd=fd, dst_dir_fd=fd)
    os.fsync(fd)


def manage(root, action, run_id, key, claim_uid):
    if action not in ('prepare', 'clean') or not re.fullmatch(r'[0-9a-f]{32}', key):
        raise ValueError('Invalid storage operation')
    if hashlib.sha256(run_id.encode()).hexdigest()[:32] != key or not claim_uid:
        raise ValueError('Invalid storage identity')
    identity = dict(run_id=run_id, key=key, claim_uid=claim_uid)
    with directory(None, root) as base, directory(base, 'lifecycle', True) as index, directory(index, key, True) as record:
        lock = os.open('lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600, dir_fd=record)
        with os.fdopen(lock, 'r+') as stream:
            fcntl.flock(stream, fcntl.LOCK_EX)
            try:
                state_fd = os.open('state.json', os.O_RDONLY | os.O_NOFOLLOW, dir_fd=record)
                with os.fdopen(state_fd) as source:
                    state = json.load(source)
            except FileNotFoundError:
                state = {**identity, 'phase': 'preparing'}
            if any(state.get(k) != v for k, v in identity.items()):
                raise ValueError('Storage ownership mismatch')
            if action == 'prepare' and state['phase'] in ('deleting', 'deleted'):
                raise ValueError('Terminal workspace cannot be recreated')
            save(record, {**identity, 'phase': 'preparing' if action == 'prepare' else 'deleting'})
            for prefix, uid in (('workspaces', 1000), ('receipts', 0)):
                with directory(base, prefix, True) as parent:
                    if action == 'prepare':
                        with directory(parent, key, True) as leaf:
                            os.fchown(leaf, uid, uid)
                            os.fchmod(leaf, 0o700)
                    else:
                        try:
                            # Reject a substituted root; rmtree also uses FD-based
                            # traversal for nested symlinks (including racing ones).
                            with directory(parent, key):
                                pass
                        except FileNotFoundError:
                            continue
                        if not shutil.rmtree.avoids_symlink_attacks:
                            raise RuntimeError('Safe directory deletion unavailable')
                        shutil.rmtree(key, dir_fd=parent)
                    os.fsync(parent)
            save(record, {**identity, 'phase': 'active' if action == 'prepare' else 'deleted'})


if __name__ == '__main__':
    manage('/runtime-root', *sys.argv[1:])
