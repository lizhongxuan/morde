#!/bin/bash
set -u

if ! /usr/local/bin/morde-aio-profile-config restore; then
    echo 'Morde profile safeguard could not restore saved configuration; refusing AIO startup.' >&2
    # gem.sh invokes this hook in a child shell. Its parent is the gem.sh process.
    # Terminating that process makes this fail closed even when AIO's hook mode
    # is configured to warn and continue.
    kill -TERM "$PPID" 2>/dev/null || true
    exit 1
fi

if ! /usr/local/bin/morde-aio-profile-config secure-workspace-root; then
    echo 'Morde could not restore the protected Workspace root; refusing AIO startup.' >&2
    kill -TERM "$PPID" 2>/dev/null || true
    exit 1
fi

if ! /usr/local/bin/morde-aio-profile-config workspace-defaults; then
    echo 'Morde could not prepare safe Workspace-backed desktop directories; refusing AIO startup.' >&2
    kill -TERM "$PPID" 2>/dev/null || true
    exit 1
fi

if ! /usr/local/bin/morde-aio-profile-config prune-browser-metrics; then
    echo 'Morde could not safely prune deferred browser metrics; refusing AIO startup.' >&2
    kill -TERM "$PPID" 2>/dev/null || true
    exit 1
fi

original_hook_file="${MORDE_AIO_ORIGINAL_PRE_HOOK_FILE:-}"
if [ -n "$original_hook_file" ] && [ -s "$original_hook_file" ]; then
    original_hook="$(cat "$original_hook_file")"
    if [ -n "$original_hook" ]; then
        bash -c "$original_hook"
        exit $?
    fi
fi
