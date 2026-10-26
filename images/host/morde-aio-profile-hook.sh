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

original_hook_file="${MORDE_AIO_ORIGINAL_PRE_HOOK_FILE:-}"
if [ -n "$original_hook_file" ] && [ -s "$original_hook_file" ]; then
    original_hook="$(cat "$original_hook_file")"
    if [ -n "$original_hook" ]; then
        bash -c "$original_hook"
        exit $?
    fi
fi
