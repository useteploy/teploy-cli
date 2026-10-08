# Managed volume ownership before startup

Existing `volumes` maps keep their meaning. A logical name resolves to
`/deployments/<app>/volumes/<name>`; absolute host bind keys stay operator managed.
Add an explicit initialization contract for an image running as a non-root user:

```yaml
volumes:
  observe-state: /var/lib/observe
volume_ownership:
  observe-state:
    uid: 10001
    gid: 10001
    mode: "0700"
```

The ownership key must match a named entry in `volumes`. UID and GID are required
numeric IDs from 0 through 4294967294. Mode is four octal digits without special
bits; world-writable modes are refused. Host paths cannot be ownership keys.

The engine provisions the directory before any app container starts, while holding
the deployment fence. Root or passwordless sudo is required on the server. Every
parent and the target must be real directories, not symlinks. An empty directory
can receive the requested owner/group/mode; an already matching directory is an
idempotent success, including when populated. A populated mismatch refuses before
startup and requires a deliberate operator migration. Initialization never performs
recursive chown/chmod and never changes existing files or subdirectories.

Accessories support the same map beside their own `volumes`:

```yaml
accessories:
  database:
    image: postgres:17
    volumes:
      data: /var/lib/postgresql/data
    volume_ownership:
      data: {uid: 999, gid: 999, mode: "0700"}
```

Accessory roots resolve below `/deployments/<app>/accessories/<accessory>/<name>`.
Explicit entries bypass the historical image-user ownership inference for that
volume. Verify the image's actual numeric UID/GID rather than copying this example
across images. Ownership is part of the reviewed plan binding and release metadata.
A pre-deploy hook runs inside a started web container, so it cannot replace this
pre-start contract.

Provisioning uses descriptor-relative, no-follow directory access. Ownership
and mode changes apply to the admitted directory descriptor, never a later
replacement of its pathname. Ancestors must belong to root or the SSH
administrator and have no group/world write permission. Root and the SSH
administrator (including that account's processes) are trusted; the app lease
does not serialize unrelated privileged administrators. Unsafe ancestors
refuse with migration guidance. The Linux target needs an existing Python 3
interpreter and root or passwordless sudo; the CLI does not install it.

All explicit app and accessory roots are checked before accessory or agent
startup and before volume migration stops writers. Running accessories also
undergo ownership admission. Explicitly owned roots bypass legacy recursive
image ownership repair. Configure these ancestor permissions deliberately when
upgrading an existing deployment tree.
