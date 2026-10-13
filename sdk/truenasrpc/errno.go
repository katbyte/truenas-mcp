package truenasrpc

// The numbers a TrueNAS gives failures of its own, beside the ones every Linux program uses: an Error's Errno holds either kind.
const (
	// ErrnoNoMethod is a method, or the part of the API it belongs to, that the NAS has not got.
	ErrnoNoMethod = 201
	// ErrnoServiceStartFailure is a service that would not start.
	ErrnoServiceStartFailure = 202
	// ErrnoAlertCheckerUnavailable is an alert the NAS could not check for just now.
	ErrnoAlertCheckerUnavailable = 203
	// ErrnoRemoteNodeError is the other NAS of a pair answering with an error.
	ErrnoRemoteNodeError = 204
	// ErrnoDatasetIsLocked is a dataset that is encrypted and has not been unlocked.
	ErrnoDatasetIsLocked = 205
	// ErrnoInvalidRRDTimestamp is a report asked for over a time the NAS has no figures for.
	ErrnoInvalidRRDTimestamp = 206
	// ErrnoNotAuthenticated is a call made with nobody logged in.
	ErrnoNotAuthenticated = 207
	// ErrnoSSLCertVerificationError is a certificate or a host key of something the NAS reaches out to that it could not check.
	ErrnoSSLCertVerificationError = 208
	// ErrnoRebootRequired is something that cannot be done until the NAS has been restarted.
	ErrnoRebootRequired = 209
	// ErrnoHAUnavailable is a pair of NASes that cannot at present take over from each other.
	ErrnoHAUnavailable = 210
)

// ErrnoName is the name a TrueNAS gives the number of a failure, "ENOENT" for 2, and "EUNKNOWN" for a number it has no name for. A TrueNAS runs
// Linux, so the numbers are Linux's wherever the program reading them runs.
func ErrnoName(number int) string {
	if name, ok := errnoNames[number]; ok {
		return name
	}

	return "EUNKNOWN"
}

var errnoNames = map[int]string{
	1: "EPERM", 2: "ENOENT", 3: "ESRCH", 4: "EINTR", 5: "EIO", 6: "ENXIO", 7: "E2BIG", 8: "ENOEXEC", 9: "EBADF", 10: "ECHILD", 11: "EAGAIN", 12: "ENOMEM",
	13: "EACCES", 14: "EFAULT", 15: "ENOTBLK", 16: "EBUSY", 17: "EEXIST", 18: "EXDEV", 19: "ENODEV", 20: "ENOTDIR", 21: "EISDIR", 22: "EINVAL", 23: "ENFILE",
	24: "EMFILE", 25: "ENOTTY", 26: "ETXTBSY", 27: "EFBIG", 28: "ENOSPC", 29: "ESPIPE", 30: "EROFS", 31: "EMLINK", 32: "EPIPE", 33: "EDOM", 34: "ERANGE",
	35: "EDEADLK", 36: "ENAMETOOLONG", 37: "ENOLCK", 38: "ENOSYS", 39: "ENOTEMPTY", 40: "ELOOP", 42: "ENOMSG", 43: "EIDRM", 60: "ENOSTR", 61: "ENODATA",
	62: "ETIME", 63: "ENOSR", 66: "EREMOTE", 71: "EPROTO", 74: "EBADMSG", 75: "EOVERFLOW", 84: "EILSEQ", 87: "EUSERS", 88: "ENOTSOCK", 89: "EDESTADDRREQ",
	90: "EMSGSIZE", 91: "EPROTOTYPE", 92: "ENOPROTOOPT", 93: "EPROTONOSUPPORT", 94: "ESOCKTNOSUPPORT", 95: "EOPNOTSUPP", 96: "EPFNOSUPPORT",
	97: "EAFNOSUPPORT", 98: "EADDRINUSE", 99: "EADDRNOTAVAIL", 100: "ENETDOWN", 101: "ENETUNREACH", 102: "ENETRESET", 103: "ECONNABORTED", 104: "ECONNRESET",
	105: "ENOBUFS", 106: "EISCONN", 107: "ENOTCONN", 108: "ESHUTDOWN", 109: "ETOOMANYREFS", 110: "ETIMEDOUT", 111: "ECONNREFUSED", 112: "EHOSTDOWN",
	113: "EHOSTUNREACH", 114: "EALREADY", 115: "EINPROGRESS", 116: "ESTALE", 122: "EDQUOT", 125: "ECANCELED", 130: "EOWNERDEAD", 131: "ENOTRECOVERABLE",

	ErrnoNoMethod: "ENOMETHOD", ErrnoServiceStartFailure: "ESERVICESTARTFAILURE", ErrnoAlertCheckerUnavailable: "EALERTCHECKERUNAVAILABLE",
	ErrnoRemoteNodeError: "EREMOTENODEERROR", ErrnoDatasetIsLocked: "EDATASETISLOCKED", ErrnoInvalidRRDTimestamp: "EINVALIDRRDTIMESTAMP",
	ErrnoNotAuthenticated: "ENOTAUTHENTICATED", ErrnoSSLCertVerificationError: "ESSLCERTVERIFICATIONERROR", ErrnoRebootRequired: "EREBOOTREQUIRED",
	ErrnoHAUnavailable: "EHAUNAVAILABLE",
}
