# Tools

truenas-mcp has 60 tools: 31 that read, 22 that write and 7 that delete. Each is named for what it acts on, then what it does: `pool_list`, `dataset_create`, `audit_space`.

A session only reads until it is told it may do more. Tools that write load with `--enable-write`, and tools that delete only with `--enable-delete` as well; a delete still removes nothing without `confirm: true`, and answers what would go. Every write takes `dry_run`.

`truenas-mcp tools` lists what the flags given would load, and needs no NAS. This page is written from the tools themselves by `go test ./cli/ -run TestTheToolsPageIsWhatIsRegistered -update`, and that test fails while it is out of date.

## Toolsets

**The default is `core` and `audit`: 12 tools that read, about 1,900 tokens.** Every tool's description takes room in a client's context before anyone asks a question, so `--toolsets` loads only the sets a session needs, and `core` always comes along.

| toolset | tools | with core | ~tokens with core |
|---|---|---|---|
| [`core`](#core) | 6 | 6 | 800 |
| [`audit`](#audit) | 6 | 12 | 1,900 |
| [`alerts`](#alerts) | 2 | 8 | 1,100 |
| [`storage`](#storage) | 3 | 9 | 1,800 |
| [`disks`](#disks) | 1 | 7 | 900 |
| [`protection`](#protection) | 20 | 26 | 4,500 |
| [`shares`](#shares) | 10 | 16 | 3,300 |
| [`services`](#services) | 6 | 12 | 1,600 |
| [`apps`](#apps) | 1 | 7 | 900 |
| [`vms`](#vms) | 1 | 7 | 900 |
| [`networking`](#networking) | 2 | 8 | 1,000 |
| [`accounts`](#accounts) | 2 | 8 | 1,100 |
| `all` | 60 | 60 | 11,000 |

Tokens are what the model sees: each tool's name, description and inputs, measured over a real `tools/list` at four bytes a token, with every tool in the set loaded. A session that only reads loads fewer.

`--toolsets` also takes a family, every tool whose name starts with it: `pool`, `dataset`, `snapshot`, `sharing` and so on. `--allow-tools` adds single tools by name and `--deny-tools` takes them away.

## core

| tool | needs | what it does |
|---|---|---|
| `server_info` | nothing | Check the NAS answers and say what it is: its TrueNAS version, whether it is ready, its name and hardware, how long it has been up, and the user the API key belongs to with what that user may do. |
| `pool_list` | nothing | List the NAS's storage pools, by name: each one's state, how full it is, how many disks it has and its last scrub. |
| `pool_get` | nothing | Describe one pool: its state and space, its last scrub, and what it is made of, down to each disk with ZFS's error counts. |
| `dataset_list` | nothing | List the NAS's datasets and volumes by their whole name, on every pool or one, or under one dataset: each one's space, quotas, compression and whether it is encrypted and locked. |
| `dataset_get` | nothing | Describe one dataset or volume: its space, down to what its snapshots and children hold, its quotas and reservations, and how it is written, encrypted and kept. |
| `alert_list` | nothing | List the alerts the NAS has raised, the gravest first: its own word on what is wrong, from a failing disk or a pool that lost data to a task that failed or a certificate running out. |

## audit

| tool | needs | what it does |
|---|---|---|
| `audit_all` | nothing | Run every audit at its usual limits and say what is wrong with the NAS, the gravest first: each finding names the audit that found it, and each audit is counted by severity. |
| `audit_pools` | nothing | Find what is wrong with the NAS's pools, the boot pool among them: a pool that is not healthy, a vdev or a disk that is not ONLINE, a disk ZFS has counted errors against, a scrub that found errors, and a pool not scrubbed for 35 days or with no scrub task. |
| `audit_space` | nothing | Find where space is running out: a pool more than 80% full, and a dataset near or at its quota, counting what is under it and its snapshots, or its refquota, counting its own data alone. |
| `audit_tasks` | nothing | Find the tasks that protect data and are not doing it: a snapshot task, replication, copy (rsync) task, cloud sync or cloud backup whose last run failed, one held, one copying a locked dataset, which a copy task reports as success, and one switched off. |
| `audit_disks` | nothing | Find the disks that are failing or too hot: a disk past the most its maker rates it for, or warmer than its kind should run, and a disk whose own SMART checks failed a self-test, report uncorrectable errors, or are running out of spare blocks. |
| `audit_system` | nothing | Find what is wrong with the NAS itself: an alert it raised at warning or graver, a certificate run out or running out within 30 days, a service set to start that is not running, a NAS that is not ready, and an update waiting or a check for one that failed. |

## alerts

| tool | needs | what it does |
|---|---|---|
| `alert_dismiss` | `--enable-write` | Dismiss an alert, so it is no longer counted among what is wrong or listed unless dismissed alerts are asked for. |
| `alert_restore` | `--enable-write` | Bring back an alert that was dismissed, so it is counted among what is wrong again. |

## storage

| tool | needs | what it does |
|---|---|---|
| `dataset_create` | `--enable-write` | Make a dataset, which holds files and is what shares and snapshot tasks are set on, or a volume: a disk of a set size to offer over iSCSI or to a virtual machine. |
| `dataset_update` | `--enable-write` | Change a dataset's or volume's quotas, compression, comments or whether it can only be read, or grow a volume. |
| `dataset_delete` | `--enable-delete` | Remove a dataset or volume and everything in it, for good: its files, its snapshots, and whatever the NAS has set on it, such as shares and tasks, which the NAS removes with it. |

## disks

| tool | needs | what it does |
|---|---|---|
| `disk_list` | nothing | List the NAS's disks: each one's serial number, model, size, the pool it is part of, and how warm it is. |

## protection

| tool | needs | what it does |
|---|---|---|
| `snapshot_list` | nothing | List one dataset's snapshots by name, with when each was taken, the space only it holds and how much data it holds. |
| `snapshottask_list` | nothing | List the tasks that take snapshots on a schedule: the dataset each takes, when it runs, how long it keeps them, whether it is switched on, and how its last run went. |
| `replication_list` | nothing | List the replications, which copy datasets' snapshots to another dataset, here or on another NAS: what each copies where, how, whether it runs by itself, and how its last run went. |
| `rsynctask_list` | nothing | List the rsync tasks, which copy a directory to or from another machine on a schedule: where each copies, when, whether it is switched on, and how its last run went. |
| `cloudsync_list` | nothing | List the cloud sync tasks, which copy a directory to or from a cloud account on a schedule: where each copies, how, when, and how its last run went. |
| `cloud_backup_list` | nothing | List the cloud backup tasks, which back a directory up to a cloud account with restic on a schedule: what each backs up where, when, how many backups it keeps, and how its last run went. |
| `pool_scrub_list` | nothing | List the scrub tasks, which read a pool's every block on a schedule to find damage before it is needed: the pool each scrubs, when, and how long it waits between scrubs. |
| `snapshot_create` | `--enable-write` | Take a snapshot of a dataset or volume: what it holds at this moment, kept until the snapshot is removed. |
| `snapshot_delete` | `--enable-delete` | Remove a snapshot for good, freeing the space only it holds: what the dataset held when it was taken can no longer be gone back to or copied from it. |
| `snapshot_rollback` | `--enable-delete` | Put a dataset back as it was when one of its snapshots was taken, for good: everything written to it since is lost, and so are its snapshots taken after it, which the NAS refuses to go back past unless remove_newer is given. |
| `snapshottask_create` | `--enable-write` | Make a task that takes a snapshot of a dataset on a schedule and removes each one once it has been kept as long as asked. |
| `snapshottask_update` | `--enable-write` | Change a snapshot task: its dataset, schedule, how long it keeps snapshots, how it names them, what it leaves out, or switch it on or off. |
| `snapshottask_delete` | `--enable-delete` | Remove a snapshot task, so no more snapshots are taken on its schedule. |
| `snapshottask_run` | `--enable-write` | Have a snapshot task take its snapshot now, as it would on its schedule, kept and named the same. |
| `pool_scrub_create` | `--enable-write` | Make the task that scrubs a pool on a schedule, reading its every block to find damage while the other copy can still mend it. |
| `pool_scrub_update` | `--enable-write` | Change a pool's scrub task: when it may start, how long it waits between scrubs, its description, or switch it on or off. |
| `pool_scrub_delete` | `--enable-delete` | Remove a pool's scrub task, so the pool is scrubbed only when someone starts one. |
| `pool_scrub_start` | `--enable-write` | Start scrubbing a pool now, reading its every block to find damage, or take up a paused scrub where it stopped. |
| `pool_scrub_pause` | `--enable-write` | Pause a pool's running scrub where it is, to spare the pool while it is busy: pool_scrub_start takes it up where it stopped. |
| `pool_scrub_stop` | `--enable-write` | Stop a pool's scrub, running or paused, for good: the next one starts from the beginning. |

## shares

| tool | needs | what it does |
|---|---|---|
| `sharing_smb_list` | nothing | List the SMB shares, which Windows, macOS and Linux machines mount: each one's name, the directory it shares, what it is set up for, and whether it is on, read only or locked. |
| `sharing_nfs_list` | nothing | List the NFS shares, which Linux and other Unix machines mount: the directory each shares, which machines may mount it, who a client's users act as, and whether it is on, read only or locked. |
| `iscsi_target_list` | nothing | List the iSCSI targets, which offer the NAS's volumes and files to other machines as disks: each one's name, the portals it is offered on and who may connect. |
| `iscsi_extent_list` | nothing | List the iSCSI extents, the volumes and files the NAS offers as disks through its targets: what each offers, and whether it is on, read only or locked. |
| `sharing_smb_create` | `--enable-write` | Share a directory over SMB, which Windows, macOS and Linux machines mount, by a name other machines see. |
| `sharing_smb_update` | `--enable-write` | Change an SMB share: its name, directory, purpose or comment, whether it may only be read, is hidden or is offered at all. |
| `sharing_smb_delete` | `--enable-delete` | Stop sharing a directory over SMB. |
| `sharing_nfs_create` | `--enable-write` | Share a directory over NFS, which Linux and other Unix machines mount, to the machines and networks named, or to any. |
| `sharing_nfs_update` | `--enable-write` | Change an NFS share: its directory or comment, which machines and networks may mount it, who a client's users act as, how clients prove who they are, whether it may only be read or is offered at all. |
| `sharing_nfs_delete` | `--enable-delete` | Stop sharing a directory over NFS. |

## services

| tool | needs | what it does |
|---|---|---|
| `service_list` | nothing | List the NAS's services, such as SMB, NFS, SSH and iSCSI: whether each is running and whether it starts when the NAS does. |
| `service_start` | `--enable-write` | Start a service now, such as SMB (cifs), NFS or SSH: a share is only served while its service runs. |
| `service_stop` | `--enable-write` | Stop a service now: machines using it lose it, such as every share it serves, until it is started again. |
| `service_restart` | `--enable-write` | Restart a service, or start it if it is stopped: machines using it are cut off for a moment, and it comes back with its settings read afresh. |
| `service_reload` | `--enable-write` | Have a running service read its settings afresh without stopping it, where it can; machines using it stay connected. |
| `service_update` | `--enable-write` | Set whether a service starts when the NAS does. |

## apps

| tool | needs | what it does |
|---|---|---|
| `app_list` | nothing | List the apps the NAS runs in containers: whether each is running, its version and whether a newer one or newer images are out, and the ports it listens on. |

## vms

| tool | needs | what it does |
|---|---|---|
| `vm_list` | nothing | List the virtual machines the NAS runs: whether each is running, whether it starts with the NAS, and the processors, memory and devices it is given. |

## networking

| tool | needs | what it does |
|---|---|---|
| `interface_list` | nothing | List the NAS's network interfaces: each one's type, whether its link is up, the addresses it has now and whether DHCP gave them, and what a bridge, link aggregation or VLAN is made of. |
| `network_configuration_get` | nothing | Say how the NAS is named and routed on the network: its hostname and domain, its gateways, and the name servers it asks, those DHCP gave included. |

## accounts

| tool | needs | what it does |
|---|---|---|
| `user_list` | nothing | List the NAS's users: each one's name, groups and what they may do through the NAS, and how they log in: password, SSH key, one-time password, and whether they are locked out. |
| `group_list` | nothing | List the NAS's groups: the users in each, what its users may do through the NAS, and whether they may use SMB or run commands as root. |
