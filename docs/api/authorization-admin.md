# Authorization and local administration foundation

## Trust boundaries

S3 requests are authenticated by header-based SigV4. That produces an
Access Key ID and principal identity; `internal/authorization` makes a
separate fixed-permission decision for the key before any bucket/object lookup.
Bucket ownership remains an additional check. A valid signature alone grants
no operation.

There is no HTTP administrative API. `/health/*` is public service health and
`/console/` is a static status UI; neither administers credentials or data.
Credential and permission administration uses `stillfsctl` from a trusted OS
account with access to PostgreSQL and the configured master key. No API grants
admin access or allows a principal to create an administrator.

## Permissions

The initial fixed capabilities are `s3:ListBuckets`, `s3:ListObjects`,
`s3:CreateBucket`, `s3:DeleteBucket`, `s3:GetObject`, `s3:PutObject`, and
`s3:DeleteObject`. New credentials are denied all operations until permissions
are explicitly granted. A credential may receive a subset even if another key
belongs to the same principal. Permissions do not bypass bucket ownership.
There are no credential-management, audit-read, or global-administration
permissions available to S3 keys. This is not AWS IAM: there are no policies,
resource patterns, conditions, roles, groups, delegation, or web administration.

Create a least-privilege credential locally, repeating only the actions it
needs:

```sh
stillfsctl credential create --permission s3:ListBuckets --permission s3:GetObject
```

Attach it to an existing principal with `--principal-id ID`. Replace a key's
permissions with `stillfsctl credential permissions set ACCESS_KEY_ID
--permission s3:GetObject`; omit permission flags to remove all grants. Revoke
with `stillfsctl credential revoke ACCESS_KEY_ID`. The secret is printed only
once on creation. Protect the local account, database role, terminal history,
process environment, and captured output. Do not pass secrets on command-line
arguments.

## Audit

Credential creation, revocation, and permission replacement append an
`audit_events` row in the same PostgreSQL transaction as the change. Audit
queries also record an `audit.read` event. Each row records the database
`current_user` role as actor, action, access-key ID target where applicable,
and database timestamp. The key ID is not the secret. No signature, encrypted
or plaintext key secret, authorization header, or object content is recorded.
Inspect events with `stillfsctl audit list [--after ID] [--limit N]` from the
trusted local account. There is no HTTP audit endpoint.

There is currently no retention/archival policy. Normal application APIs only
append events, but a PostgreSQL owner can update or delete them; these records
are not immutable or tamper-proof. Restrict database role access and include
the table in protected backups.

## Credential encryption key

Credential secrets remain encrypted as AES-256-GCM ciphertext with the Access
Key ID as associated data. `STILLFS_CREDENTIAL_ENCRYPTION_KEY` accepts the
legacy Base64 form of 32 random bytes. Prefer
`STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE`, which points to a protected regular
file containing exactly 32 raw bytes and mode `0600` (no group/other access).
Set only one source. Neither key value belongs in source control, Compose,
logs, shell history, command arguments, or container images.

Migration `0004_credential_master_key_state.sql` adds a fixed HMAC verifier
and generation in PostgreSQL; it does not change credential ciphertext. The
server checks the configured key at startup, and credential reads check the
verifier in the same SQL statement as the ciphertext. Credential creation,
permission changes, revocation, verification, and rotation use a shared
transaction advisory lock. A stale process therefore cannot create a new
credential encrypted under the previous key after rotation commits.

### Back up and rotate

1. Back up PostgreSQL and the exact current key together. Store the database
   backup and a separately encrypted key backup under restricted access; test
   restoring both to an isolated environment. Do not print the key while
   validating retrieval.
2. Stop StillFS and other credential-management processes. Use one operator
   process and one StillFS instance; the advisory lock serializes StillFS
   credential operations, but it cannot fence old binaries or direct SQL.
3. Place the current and replacement keys in separate protected files, each
   exactly 32 raw bytes and mode `0600`. Use a secret manager or protected
   file-transfer mechanism to populate them; do not put bytes in shell
   arguments or command output.
4. Run the local command with database configuration available:

   ```sh
   stillfsctl credential rotate-master-key \
     --old-key-file /secure/stillfs/master-old.key \
     --new-key-file /secure/stillfs/master-new.key
   ```

   The command re-encrypts all credential rows, verifies each new ciphertext,
   updates the key verifier and writes the success audit event in one
   PostgreSQL transaction. It prints only the number of rotated credentials.
5. Point `STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE` at the new file, verify it,
   and then restart StillFS:

   ```sh
   stillfsctl credential verify-master-key --key-file /secure/stillfs/master-new.key
   ```

   Keep the previous key protected until PostgreSQL and S3 authentication have
   been verified and the new backup pair has been tested. Retire the old key
   through the site's secret-management procedure.

### Failure and recovery

The database re-encryption and active-key verifier update commit together. A
failure while reading/decrypting any credential, encrypting/verifying a new
value, or updating PostgreSQL rolls back the entire credential set. The old
key remains active and a retry with the same old/new files is safe. Verify the
old key before retrying. No plaintext is written to temporary files.

If the client loses its connection during commit, the result can be ambiguous.
Inspect `credential.master-key-rotation.*` events and run
`stillfsctl credential verify-master-key --key-file FILE` with each candidate.
The verifier checks every row; only the key matching all credential ciphertext
is usable. A crash can leave only the `started` event if the transaction
rolled back before it could write its terminal event. Audit rows may be
operator-edited, so the verifier, not the audit history alone, decides which
key is active. Do not restart the server until the active key is verified.

If the key is lost, restore the matching PostgreSQL backup and key backup.
Without either the original key or a matching key backup, encrypted credential
secrets cannot be recovered; there is no key-reset command. Object bytes and
object metadata are unaffected by rotation.

The CLI records started, failed, and successful rotation events without key
material. Success is part of the rotation transaction; failures are recorded
after rollback when PostgreSQL remains available. A database outage can prevent
the failure event from being persisted. Audit rows are not immutable and have
no retention policy.

### Credential recovery and migration 0003

Migration `0003_authorization_audit.sql` intentionally grants no permissions
to existing credentials. Grant only the required actions explicitly, for
example:

```sh
stillfsctl credential permissions set ACCESS_KEY_ID \
  --permission s3:ListObjects --permission s3:GetObject
```

Provision a replacement key for a known principal with explicit permissions:

```sh
stillfsctl credential create --principal-id PRINCIPAL_ID \
  --permission s3:ListBuckets --permission s3:GetObject
```

The new secret is printed once. Revocation remains available even when the
master key is unavailable: `stillfsctl credential revoke ACCESS_KEY_ID`.
Creating replacement keys still requires the active master key; if it is lost,
restore a matching database/key backup first.
