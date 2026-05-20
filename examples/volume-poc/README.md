# volume-poc — substrate verification + attack matrix

Working demo of creekd's Volume + VolumeMount substrate (RFC
extension 1), plus a labeled attack matrix that exercises every
hardening path identified in the multi-agent pentest review.

## What this proves

1. **Happy path** — `up.sh` registers a Volume, spawns a sandboxed
   toy app with the Volume bind-mounted at `/data`, writes through
   the bind, kills the app, restarts it, reads the data back. Data
   persists across process restarts.

2. **Attack matrix** — `attacks.sh` sends 12 crafted admin-API
   requests that a compromised orchestrator might send. Each must
   be refused with the expected error.

## Requirements

- Linux (bind mount + namespaces are kernel-specific)
- Root (CAP_SYS_ADMIN for mounts + cgroups)
- Go toolchain
- `curl`, `jq`, `awk` (standard on any Linux distro)
- Kernel ≥5.12 for the atomic mount path; older kernels run the
  legacy fallback (with documented hardening gaps — see
  `internal/supervisor/mount_atomic_linux.go`)

On macOS, run inside the test container:

```bash
docker build -f ../../Dockerfile.test -t creekd-test:dev ../..
docker run --rm -it --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -w /work/examples/volume-poc creekd-test:dev ./up.sh
```

## Run it

```bash
sudo ./up.sh         # builds binaries, starts creekd, registers + spawns
sudo ./attacks.sh    # runs the attack matrix
sudo ./down.sh       # cleans up mounts, kills creekd, removes scratch
```

## Layout

```
volume-poc/
├── README.md              # you are here
├── up.sh                  # happy path
├── attacks.sh             # security validation
├── down.sh                # cleanup
└── toy/main.go            # tenant-side HTTP service that reads/writes /data
```

## Attack matrix

Each attack is labeled to the pentest finding it covers. Refer to
the multi-agent review summary in commit `d1497b1` and the related
hardening commits.

| ID  | Class               | What's tried                                                  | Expected block                              |
|-----|---------------------|---------------------------------------------------------------|---------------------------------------------|
| A1  | Path traversal      | `RegisterVolume backing_path: "../etc"`                       | API rejects with "contains '..'"            |
| A2  | Path traversal      | `RegisterVolume backing_path: "/etc/passwd"` (absolute)       | API rejects with "must be relative"         |
| A3  | Symlink escape      | Plant `evil-symlink → /etc` inside VolumeRoot, register it    | openat2 `RESOLVE_NO_SYMLINKS` refuses       |
| A4  | Host-target overlay | `VolumeMount target: "/etc/passwd"` without chroot            | AllowedTargetPrefixes denies                |
| A5  | Host-target overlay | `VolumeMount target: "/proc/anything"` without chroot         | Forbidden-prefix denylist refuses           |
| A6  | Sandbox bypass      | `Sandbox.Chroot: "/"` (would bypass allowlist)                | Validation rejects "chroot must not be /"   |
| A7  | SubPath traversal   | `sub_path: "../neighbor"`                                     | Validation rejects "contains '..'"          |
| A8  | SubPath traversal   | `sub_path: "/abs"` (absolute)                                 | Validation rejects "must be relative"       |
| A9  | Reference           | `VolumeMount volume_id: "ghost"` (never registered)           | Spawn rejects "volume not found"            |
| A10 | Lifecycle           | `DELETE /v1/volumes/vol-a` while an app references it         | 409 "still referenced"                      |
| A11 | Duplicate target    | Two VolumeMounts with the same `target` in one Config         | Validation rejects "duplicate target"       |
| A12 | Live data probe     | Tenant process tries to read `/etc/passwd` from the host      | Chroot + mount-namespace block the read     |

A12 is the only "live" attack — it asks the running sandboxed toy
process to try reading the host's `/etc/passwd`. With Sandbox.Chroot
set, `/etc` doesn't exist inside the tenant's filesystem view.

## Mapping to pentest findings

| Attack | Pentest finding                                                           |
|--------|---------------------------------------------------------------------------|
| A1, A2 | Path traversal (`containsDotDot` + `filepath.IsAbs` in `RegisterVolume`)  |
| A3     | Symlink escape (`openat2(RESOLVE_NO_SYMLINKS)`)                           |
| A4, A5 | H7: AllowedTargetPrefixes + forbidden-prefix denylist                     |
| A6     | C5: `Sandbox.Chroot = "/"` rejected                                       |
| A7, A8 | SubPath validation                                                        |
| A9     | Reference integrity (`ErrVolumeNotFound`)                                 |
| A10    | C4: `UnregisterVolume` actually counts refs                               |
| A11    | Target-uniqueness check                                                   |
| A12    | Default-on `MountNamespace` + `Sandbox.Chroot` containment                |

## What this PoC does NOT cover

- Kernel-version dependent flags (MS_NOSUID/MS_NODEV on sub-mounts) —
  these are exercised by Linux integration tests in
  `internal/supervisor/`. They require crafted nested-mount setups
  that don't make sense in a single-host demo.
- DoS vectors (admin API flood, mount-table exhaustion) — covered
  by `MaxBytesReader` + server timeouts; benchmarking those is a
  separate Phase 1 readiness item, not a security demo.
- Per-tenant UID isolation — Phase 2 work (idmap mounts +
  default-on user namespace). Today's threat model assumes the
  operator trusts the orchestrator that calls the admin API; a
  malicious orchestrator with a valid bearer token can DoS but
  cannot escape the documented validation surface.

## Reproducing the pentest review

The multi-agent pentest that drove these hardening choices is
documented inline in the commit messages (`d1497b1` and `8e06233`).
Each finding is mapped to a specific code reference in the file
that fixed it.
