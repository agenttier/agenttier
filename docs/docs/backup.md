# Backup and restore

AgentTier sandbox workspaces live on PersistentVolumeClaims. Two layers protect
them: in-cluster scheduled VolumeSnapshots (Layer 1, shipped) and out-of-cluster
export (Layer 2, recipe).

## Why

- **Stuck ReadWriteOnce volume.** A node dies with a sandbox PVC attached.
  AZ-pinned EBS volumes can take several minutes to fail over; a recent snapshot
  gives you a recovery path.
- **No "oops" recovery.** Deleting a sandbox deletes its PVC. A backup snapshot
  lets you restore the workspace into a new sandbox.

## Layer 1 — scheduled VolumeSnapshots (in-cluster)

Opt-in via Helm. The controller snapshots every managed, non-pool sandbox PVC on
an interval and prunes snapshots older than the retention window:

```yaml
optional:
  backup:
    snapshots:
      enabled: true
      intervalHours: 6      # snapshot cadence
      retentionDays: 14     # prune backups older than this
      # snapshotClassName: ""   # empty uses the cluster default VolumeSnapshotClass
```

It runs as a leader-elected loop inside the controller (no extra CronJob or
image), reusing the same `VolumeSnapshotClass` and CSI snapshotter the
[cloning](cloning.md) feature depends on. Backups are labelled
`agenttier.io/snapshot-kind=scheduled-backup`, so the retention sweep only ever
prunes its own snapshots — never clone or snapshot-on-stop snapshots.

List a sandbox's backups:

```bash
kubectl get volumesnapshots -n agenttier \
  -l agenttier.io/snapshot-kind=scheduled-backup,agenttier.io/source-pvc=<pvc-name>
```

### Restore

Restore is the existing `spec.cloneFromSnapshot` path — create a new sandbox
whose PVC is provisioned from a backup snapshot:

```yaml
apiVersion: agenttier.io/v1alpha1
kind: Sandbox
metadata:
  name: restored
  namespace: agenttier
spec:
  templateRef: { name: general-coding, kind: ClusterSandboxTemplate }
  cloneFromSnapshot: <backup-snapshot-name>
```

The controller stamps the PVC's `dataSource` with the VolumeSnapshot and the CSI
driver hydrates the volume from it. The snapshot must be in the same namespace.

## Layer 2 — out-of-cluster export (recipe)

For off-cluster retention (S3, cross-region, long-term), two options:

- **Velero** — the battle-tested choice for enterprise. Install Velero with the
  CSI plugin, then a `Schedule` that backs up the sandbox namespace with
  volume snapshots uploaded to your object store. Velero owns lifecycle,
  retention, and cross-cluster restore.
- **Custom `aws s3 sync`** — for solo operators who want minimal dependencies:
  a one-shot Job that mounts each PVC read-only and `aws s3 sync /workspace
  s3://<bucket>/<sandbox>/` on a schedule. ~30 lines, no new controller.

Layer 2 is intentionally not bundled into the chart — it's an operator choice
with real cost and dependency trade-offs. Pick Velero for serious DR; the S3
snippet for lightweight retention.

## REST API, SDK, and CLI

Everything above is also reachable without `kubectl` — `GET/POST/DELETE
/api/v1/sandboxes/{id}/backups*`, `client.sandboxes` / `sandbox.backups` in
the Python SDK, and `agenttier sandbox backups {list,create,restore,delete}`
in both CLIs. On-demand backups created this way are labeled identically to
scheduled ones, so retention pruning still applies. See
[Backups](api/new-endpoints.md#backups-apiv1sandboxesidbackups) for the full
wire contract.

## Acceptance check

Kill an EKS node hosting a sandbox; confirm the sandbox PVC has a snapshot from
within the retention window, then restore it into a new sandbox with
`cloneFromSnapshot` and verify the files are intact.
