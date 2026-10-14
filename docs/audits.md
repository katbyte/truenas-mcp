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
