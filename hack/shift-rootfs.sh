#!/bin/sh
# Shift every uid and gid in a root filesystem image up by the sandbox's
# mapped base, so the files belong to sandbox root and its users rather than
# to host root. The overlay refuses to copy up a file whose owner the sandbox
# cannot name (EOVERFLOW), and host root is exactly the id its map leaves out.
#
# The base is mappedBase in internal/sandbox/sandbox_linux.go; the test suite
# fails with a pointer here if the two drift. Idempotent: ids already at or
# above the base are left alone, so an image can be shifted twice safely.
#
#   sudo hack/shift-rootfs.sh /var/tmp/forge-rootfs
set -eu

rootfs=$1
base=${2:-524288}

find "$rootfs" -exec stat -c '%u %g %n' {} + | while read -r uid gid path; do
	if [ "$uid" -lt "$base" ]; then
		chown -h "$((uid + base)):$((gid + base))" "$path"
	fi
done
