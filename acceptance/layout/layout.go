// Package layout is what the throwaway TrueNAS the live tests run against is made to hold: the program that sets one up goes by it, and so do the
// tests that expect to find it. Change something here and both change with it.
package layout

// Hostname is the name the test NAS is given. A live test refuses to run against a NAS of any other name, which is how it knows the NAS in front
// of it is the one that was made to be thrown away.
const Hostname = "throwaway-nas"

// AdminUser is the administrator a TrueNAS is installed with, the one name its installer offers beside root, and ReadOnlyUser a second user, who
// may read everything and change nothing.
const (
	AdminUser    = "truenas_admin"
	ReadOnlyUser = "readonly"
)

// Pool is the one pool the test NAS has: a mirror of the two disks whose serials are DiskSerial with 1 and with 2 after it. The disks numbered 3
// and 4 are left unused, for a test to make a pool of its own from. HotDiskSerial is a fifth, also unused, of another kind: an NVMe disk, which
// says how hot it is where the others say nothing.
const (
	Pool          = "tank"
	DiskSerial    = "TNTEST"
	HotDiskSerial = DiskSerial + "NVME"
)

// Datasets are the datasets in the pool, by their full names.
var Datasets = []string{Pool + "/alpha", Pool + "/beta"}

// A furnished test NAS has, beside all that, one of each thing a NAS is watched for, so that whatever watches one has something to see: a user
// who may use SMB, a share of each kind on a dataset of its own, an iSCSI target, a virtual machine that is described and not running, and a
// task of each kind that needs nothing outside the NAS, in each state a task can be left in. The snapshot task has run and finished, the
// replication has never run, and the copy task was run and failed, since where it copies to is not there.
const (
	SMBUser        = "sharer"
	SMBShare       = "shared"
	SMBDataset     = Pool + "/shared-smb"
	NFSDataset     = Pool + "/shared-nfs"
	Target         = "exported"
	Machine        = "small"
	SnapshotSchema = "counted-%Y-%m-%d_%H-%M"
	Replication    = "a copy that has not been made"
	CopyTask       = "a copy to nowhere"
)

// TaskDataset is the dataset the furnished NAS's tasks are about, and ReplicationTarget where its replication would copy it to.
var (
	TaskDataset       = Datasets[1]
	ReplicationTarget = Pool + "/copied"
)
