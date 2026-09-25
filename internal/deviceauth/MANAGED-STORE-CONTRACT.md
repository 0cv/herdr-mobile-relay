# Managed device-store writer contract

`Store` mutations share one cooperating-writer boundary across store instances
and processes. On Linux and macOS, a writer takes a nonblocking exclusive
`flock` on the stable
`<runtime-parent>/.herdr-deviceauth-writer.lock` file. The lock file is opened
without following symlinks, must be a current-user-owned regular file with one
link and mode `0600`, and is never chmodded, replaced, or removed when it
already exists. Its parent must be a real current-user-owned directory that is
not group/world writable. A busy lock fails immediately; writers do not wait
indefinitely. Other platforms explicitly refuse writes rather than silently
falling back to an uncoordinated writer.

The lock file is created only when a write is authorized. `OpenDeferred` stays
read-only: it does not create/chmod the device directory, store file, or lock
file. A normal `Open` takes the lock before protecting/creating the device
store. It loads an existing valid file without rewriting it merely to
initialize a `Store`. Invitation, bootstrap, reset, authentication/enrollment,
rename, revoke, and every other `persistLocked` mutation use the same lock.
Managed transactional bootstrap arm holds it across baseline validation,
installation, final admission and any exact rollback.

Every opened Store retains a bounded snapshot of the device directory and file
identity, mode and bytes. A writer checks that snapshot while exclusively
locked before replacing the file. After every successful store-owned durable
write, including ordinary authentication/enrollment and credential management,
the snapshot is refreshed to the inode and bytes actually installed. An older
Store instance therefore refuses to overwrite a newer cooperating writer;
managed reprint can reuse the same Store after enrollment, rename, or revoke
without treating those store-owned changes as foreign state. A mismatch is
never adopted or reset. Managed rollback removes/restores only an object whose
identity is still proven to be the transaction's own; if it cannot prove and
restore the exact prior bytes, mode, or absence, it returns
`ErrManagedArmRecovery` and preserves the replacement for operator recovery.

For managed arm, the durable file replacement and directory sync precede the
final admission callback. While the gate's resolver lock excludes new auth,
that callback rechecks the real authority guard and owner, then applies the Hub
admission transition under its registration barrier. The gate marks the
invitation open only after that callback succeeds and its final live-authority
check passes. Failure rolls the device-store transaction back; a lost control
acknowledgement after the successful callback is committed/ambiguous and never
rolls back an invitation or enrolled credential that may have been observed.

The guarantee is explicitly limited to cooperating writers that use this
lock. Portable Go/filesystem APIs do not provide a conditional rename by
expected inode. An arbitrary same-UID process that ignores the lock can replace
the pathname after the final snapshot comparison but before `os.Rename`; that
raw check/rename race cannot be ruled out. Detected mismatches, including a
replacement visible at the managed `before-rename` test seam, are refused, and
rollback never deliberately deletes a foreign/replaced path, but no atomic
same-UID compare-and-swap or unconditional pathname-preservation claim is made.
