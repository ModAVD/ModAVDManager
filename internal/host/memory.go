package host

// ProcessMemory is the memory of one process, in bytes.
type ProcessMemory struct {
	Commit     uint64 // private commit charge (Task Manager's "Commit size")
	PeakCommit uint64 // the highest Commit has been
	WorkingSet uint64 // in physical memory now
}

// SystemMemory is the memory of the computer, in bytes.
type SystemMemory struct {
	TotalPhys   uint64
	AvailPhys   uint64
	CommitLimit uint64 // physical memory plus page files
	Committed   uint64
}
