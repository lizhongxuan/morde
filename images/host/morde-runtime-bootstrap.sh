#!/bin/sh

morde_bootstrap_runtime_credential() {
    [ "$(id -u)" -eq 0 ] || { echo 'Morde runtime bootstrap must run as root' >&2; return 1; }
    case "${MORDE_RUNTIME_BOOTSTRAP_KEY:-}" in
        [A-Za-z0-9_-][A-Za-z0-9_-][A-Za-z0-9_-][A-Za-z0-9_-]*) ;;
        *) echo 'Morde runtime credential is missing or invalid' >&2; return 1 ;;
    esac
    [ "${#MORDE_RUNTIME_BOOTSTRAP_KEY}" -eq 43 ] || {
        echo 'Morde runtime credential has an invalid length' >&2
        return 1
    }
    case "$MORDE_RUNTIME_BOOTSTRAP_KEY" in
        *[!A-Za-z0-9_-]*) echo 'Morde runtime credential contains invalid characters' >&2; return 1 ;;
    esac

    original_umask="$(umask)"
    umask 077
    install -d -o root -g root -m 0700 /run/morde-runtime
    chmod 0700 /run/morde-runtime
    install -d -o root -g root -m 0700 /var/lib/morde-runtime
    chmod 0700 /var/lib/morde-runtime
    [ ! -L /var/lib/morde-runtime/cache ] || { echo 'Morde sync cache path is unsafe' >&2; return 1; }
    install -d -o root -g root -m 0700 /var/lib/morde-runtime/cache
    [ "$(stat -c '%u:%g:%a' /var/lib/morde-runtime/cache)" = '0:0:700' ] || {
        echo 'Morde sync cache permissions are unsafe' >&2
        return 1
    }
    temporary="$(mktemp /run/morde-runtime/.credential.XXXXXX)"
    if ! printf '%s' "$MORDE_RUNTIME_BOOTSTRAP_KEY" > "$temporary"; then
        rm -f "$temporary"
        return 1
    fi
    chown root:root "$temporary"
    chmod 0400 "$temporary"
    mv -f "$temporary" /run/morde-runtime/credential
    umask "$original_umask"
    unset MORDE_RUNTIME_BOOTSTRAP_KEY
}

morde_bootstrap_host_replica_credential() {
    [ "$(id -u)" -eq 0 ] || { echo 'Morde Host replica bootstrap must run as root' >&2; return 1; }
    case "${MORDE_HOST_REPLICA_KEY:-}" in
        [A-Za-z0-9_-][A-Za-z0-9_-][A-Za-z0-9_-][A-Za-z0-9_-]*) ;;
        *) echo 'Morde Host replica credential is missing or invalid' >&2; return 1 ;;
    esac
    [ "${#MORDE_HOST_REPLICA_KEY}" -eq 43 ] || {
        echo 'Morde Host replica credential has an invalid length' >&2
        return 1
    }
    case "$MORDE_HOST_REPLICA_KEY" in
        *[!A-Za-z0-9_-]*) echo 'Morde Host replica credential contains invalid characters' >&2; return 1 ;;
    esac

    original_umask="$(umask)"
    umask 077
    temporary="$(mktemp /run/morde-runtime/.replica-credential.XXXXXX)"
    if ! printf '%s' "$MORDE_HOST_REPLICA_KEY" > "$temporary"; then
        rm -f "$temporary"
        return 1
    fi
    chown root:root "$temporary"
    chmod 0400 "$temporary"
    mv -f "$temporary" /run/morde-runtime/replica-credential
    umask "$original_umask"
    unset MORDE_HOST_REPLICA_KEY
}
