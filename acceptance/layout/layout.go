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
// and 4 are left unused, for a test to make a pool of its own from.
const (
	Pool       = "tank"
	DiskSerial = "TNTEST"
)

// Datasets are the datasets in the pool, by their full names.
var Datasets = []string{Pool + "/alpha", Pool + "/beta"}
