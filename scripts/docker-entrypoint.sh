#!/bin/bash
set -e

# ─── Fix ownership of bind-mounted directories ───
# When users bind-mount host directories (e.g. ./skills/preloaded),
# the mount inherits the host UID/GID which may differ from the
# container's appuser. This entrypoint runs as root, fixes ownership,
# then drops privileges to appuser via gosu — the same pattern used
# by official postgres/redis images.

# Directories that may be bind-mounted and need appuser access
MOUNT_DIRS=(
    /app/skills/preloaded
    /app/skills/professional
    /data/files
)

for dir in "${MOUNT_DIRS[@]}"; do
    if [ -d "$dir" ]; then
        chown -R appuser:appuser "$dir" 2>/dev/null || true
    fi
done

# docker.sock keeps the host daemon's numeric group ID. The image cannot know
# that GID at build time, so make appuser a member before gosu drops root.
# Mounting the socket already grants host-daemon authority; this only makes the
# configured Docker sandbox usable without running the whole app as root.
DOCKER_SOCKET="/var/run/docker.sock"
if [ -S "$DOCKER_SOCKET" ]; then
    socket_gid="$(stat -c '%g' "$DOCKER_SOCKET")"
    app_gid="$(id -g appuser)"
    if [ "$socket_gid" != "$app_gid" ]; then
        socket_group="$(getent group "$socket_gid" | cut -d: -f1 || true)"
        if [ -z "$socket_group" ]; then
            socket_group="weknora-docker-${socket_gid}"
            groupadd -g "$socket_gid" "$socket_group"
        fi
        usermod -aG "$socket_group" appuser
    fi
fi

# ─── Merge built-in skills into preloaded ───
# Built-in skills are backed up at /app/skills/_builtin during image build.
# After a bind-mount replaces /app/skills/preloaded, copy back any
# missing built-in skills (without overwriting user-provided ones).
BUILTIN_DIR="/app/skills/_builtin"
PRELOADED_DIR="/app/skills/preloaded"

if [ -d "$BUILTIN_DIR" ]; then
    mkdir -p "$PRELOADED_DIR" 2>/dev/null || true
    write_probe="$PRELOADED_DIR/.weknora-write-probe.$$"
    if touch "$write_probe" 2>/dev/null; then
        rm -f "$write_probe"
        for skill_dir in "$BUILTIN_DIR"/*/; do
            [ -d "$skill_dir" ] || continue
            skill_name="$(basename "$skill_dir")"
            if [ ! -d "$PRELOADED_DIR/$skill_name" ]; then
                cp -r "$skill_dir" "$PRELOADED_DIR/$skill_name"
            fi
        done
        chown -R appuser:appuser "$PRELOADED_DIR" 2>/dev/null || true
    else
        echo "Using read-only pre-provisioned skills at $PRELOADED_DIR"
    fi
fi

# ─── Drop privileges and exec the main process ───
exec gosu appuser "$@"
