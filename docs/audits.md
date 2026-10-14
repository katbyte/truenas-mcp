# Audits

Each audit only reports. Nothing on the NAS is changed by one. Every finding has a severity, a one-word check that `by_check` counts, what it is about, and a sentence saying what it means.

- **critical**: data is at risk or already lost.
- **warning**: it will be, if left.
- **notice**: worth knowing, and harming nothing yet.

`checked` says how many things an audit looked at. No findings beside nothing checked means there was nothing to look at, not that all is well.

## audit_pools

Looks at every pool, the boot pool among them. A damaged pool can still read ONLINE, so it goes by the pool's health and by each vdev and disk.

| check | severity | found when |
|---|---|---|
| `pool_unhealthy` | critical | the NAS says the pool is not healthy, whatever its status reads |
| `vdev_not_online` | critical | a mirror or RAIDZ group is not ONLINE: it has lost a disk |
| `disk_not_online` | critical | a disk is not ONLINE, or AVAIL or INUSE for a spare. A disk that is gone names the disk last seen there |
| `scrub_found_errors` | critical | the last scrub or resilver found errors |
| `disk_errors` | warning | ZFS has counted read, write or checksum errors against a disk since the pool was imported |
| `never_scrubbed` | warning | the pool has never been scrubbed |
| `scrub_overdue` | warning | the last scrub ended more than `scrub_days` ago, 35 unless given |
| `no_scrub_task` | warning | no scrub task for the pool is switched on. The boot pool is scrubbed by its own setting, so it is not asked |
| `pool_warning` | notice | the NAS has a warning about the pool, often that its features can be upgraded |

Tested on a real NAS by taking a disk of the mirror offline and putting it back.

## audit_space

Looks at every pool and every dataset. Each dataset is asked for its sizes alone, so a large NAS is read lightly.

| check | severity | found when |
|---|---|---|
| `pool_full` | critical | a pool is 95% full or more |
| `pool_filling` | warning | a pool is `pool_percent` full or more, 80 unless given |
| `quota_full` | critical | a dataset uses 95% of its quota or more, counting everything under it and its snapshots |
| `quota_filling` | warning | it uses `quota_percent` of its quota or more, 90 unless given |
| `refquota_full` | critical | a dataset uses 95% of its refquota or more, counting its own data alone |
| `refquota_filling` | warning | it uses `quota_percent` of its refquota or more |

Tested on a real NAS by filling a dataset to 97% of its refquota.

## audit_tasks

Looks at every task that protects data: snapshot tasks, replications, copy (rsync) tasks, cloud syncs and cloud backups.

| check | severity | found when |
|---|---|---|
| `snapshot_task_failed`, `replication_failed` | critical | the replication engine says the task is in ERROR |
| `copy_task_failed`, `cloud_sync_failed`, `cloud_backup_failed` | critical | the task's last run failed or was aborted |
| `snapshot_task_held`, `replication_held` | warning | the task is held and does not run, such as for a locked dataset |
| `task_on_locked_dataset` | warning | a copy task, cloud sync or cloud backup copies a locked dataset. A copy task run on one ends as a success, having copied nothing, so this is the only place it shows |
| `snapshot_task_off`, `replication_off`, `copy_task_off`, `cloud_sync_off`, `cloud_backup_off` | notice | the task is switched off |

A task that is switched off and failed when it last ran is told of both ways. Tested on a real NAS by running a copy task to a place that is not there.

## audit_disks

Looks at every disk's temperature, and at what the disks' own SMART checks have told the NAS. Errors ZFS counts against a disk are in `audit_pools`.

| check | severity | found when |
|---|---|---|
| `disk_too_hot` | critical | a disk is at or past the most its maker rates it for, or, for one that names no limit, 10°C past the warning for its kind |
| `disk_hot` | warning | a disk is at or past `hdd_celsius` for a spinning disk, 50 unless given, or `ssd_celsius` for a solid state or NVMe disk, 70 unless given |
| `smart_failedselftest` | warning | the disk failed a SMART self-test |
| `smart_uncorrectederrors` | warning | the disk reports errors it could not correct |
| `smart_spareblockcount` | critical | a solid state disk is running out of spare blocks |
| `smart_erasecyclecount` | warning | a solid state disk has been written over many times |

The SMART checks come from the alerts the NAS raises from them; a dismissed one is not told of. TrueNAS 25.10 has no other way to read a disk's SMART state. Tested on a real NAS with the test machine's NVMe disk, which reports its temperature.
