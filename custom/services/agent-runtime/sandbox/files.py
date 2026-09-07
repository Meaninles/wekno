"""Unprivileged, symlink-safe transfers between Docker transport and workspace."""
import contextlib
import os
import shutil
import stat
import sys
import tarfile
from pathlib import PurePosixPath

LIMIT = 128 * 1024**2


@contextlib.contextmanager
def workspace_file(path, *, write=False):
    parts = PurePosixPath(path).parts
    if parts[:2] != ("/", "workspace") or ".." in parts or len(parts) < 3:
        raise ValueError("Invalid workspace path")
    directory = os.open("/workspace", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    descriptor = None
    try:
        for part in parts[2:-1]:
            if write:
                try:
                    os.mkdir(part, mode=0o700, dir_fd=directory)
                except FileExistsError:
                    pass
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory)
            directory = child
        flags = os.O_NOFOLLOW | os.O_NONBLOCK | (os.O_WRONLY | os.O_CREAT if write else os.O_RDONLY)
        descriptor = os.open(parts[-1], flags, 0o600, dir_fd=directory)
        metadata = os.fstat(descriptor)
        if not stat.S_ISREG(metadata.st_mode) or (not write and metadata.st_size > LIMIT):
            raise ValueError("Expected a bounded regular workspace file")
        if write:
            os.ftruncate(descriptor, 0)
        with os.fdopen(descriptor, "wb" if write else "rb") as stream:
            descriptor = None
            yield stream
    finally:
        os.close(directory)
        if descriptor is not None:
            os.close(descriptor)


def main():
    mode, *args = sys.argv[1:]
    if mode == "read":
        with workspace_file(args[0]) as source:
            shutil.copyfileobj(source, sys.stdout.buffer)
    elif mode == "write-stream":
        with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as bundle:
            for member in bundle:
                if not member.isfile() or member.size > LIMIT:
                    raise ValueError("Invalid workspace transfer entry")
                with workspace_file("/workspace/"+member.name, write=True) as target:
                    with bundle.extractfile(member) as source:
                        shutil.copyfileobj(source, target)
                    target.flush()
                    os.fsync(target.fileno())
    else:
        raise ValueError("Unknown transfer mode")


if __name__ == "__main__":
    main()
