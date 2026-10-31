package supervisor

// File budget constants belong to the sealed file-load experiment. They are
// not a general disk quota and they are not defaults for CPU/RSS/Wall loads.
const (
	FileBudgetCandidate = "unique-inode-allocated-bytes"
	FileScanScope       = "lstat-nofollow-private-tree"
	// FileBudgetAlgorithm is the policy fingerprint. Directory entry order is
	// the contract: logical length, allocated space, then live name count.
	FileBudgetAlgorithm = "filebudget.v1 lstat-nofollow; candidate=sum of actual allocated bytes once per regular (dev,inode): Darwin ATTR_FILE_DATAALLOCSIZE via getattrlist(FSOPT_NOFOLLOW), otherwise st_blocks*512; stat_blocks=sum(st_blocks*512) once per regular inode and is not the Darwin budget; logical_named=sum(st_size) per regular directory entry and is not the budget; logical_unique=sum(st_size) once per regular inode; entries=live directory entries excluding the scan root; symlinks are entries and are never followed; hard links share one inode; a rename is the same inode under the new live name; unlinked open files are outside this scan"

	MaxFileBudgetBytes   = 256 << 10
	MaxFileBudgetEntries = 64

	// Helper-fixed payload bounds. Callers cannot raise these.
	FileDenseChunk    = 16 << 10
	FileGrowCap       = 128 << 10
	FileMultiCount    = 6
	FileMultiEach     = 32 << 10
	FileEntryCount    = 16
	FileSmallCount    = 2
	FileSmallEach     = 16 << 10
	FileUnlinkNamed   = 16 << 10
	FileUnlinkExtra   = 96 << 10
	FileSparseLogical = 1 << 20
	FileEdgeEach      = 16 << 10
)

// FileScan is one lstat walk of the private work root. Followed is always
// false: this scanner does not descend through or stat symlink targets.
type FileScan struct {
	Scope         string `json:"scope,omitempty"`
	LogicalNamed  uint64 `json:"logical_named_bytes,omitempty"`
	LogicalUnique uint64 `json:"logical_unique_bytes,omitempty"`
	Allocated     uint64 `json:"allocated_bytes,omitempty"`
	StatBlocks    uint64 `json:"stat_blocks_bytes,omitempty"`
	Entries       uint64 `json:"entries,omitempty"`
	UniqueInodes  uint64 `json:"unique_inodes,omitempty"`
	Symlinks      uint64 `json:"symlinks,omitempty"`
	Followed      bool   `json:"followed_symlinks,omitempty"`
}

// FileCheckpoint is one sealed helper observation. Scan fields are the
// helper's own walk at that instant; host samples are recorded separately.
type FileCheckpoint struct {
	Phase           string `json:"phase"`
	Written         uint64 `json:"written"`
	Dev             uint64 `json:"dev,omitempty"`
	Ino             uint64 `json:"ino,omitempty"`
	Live            bool   `json:"live"`
	NLink           uint64 `json:"nlink,omitempty"`
	Unlinked        bool   `json:"unlinked"`
	Closed          bool   `json:"closed"`
	LogicalNamed    uint64 `json:"logical_named"`
	LogicalUnique   uint64 `json:"logical_unique"`
	Allocated       uint64 `json:"allocated"`
	StatBlocks      uint64 `json:"stat_blocks,omitempty"`
	Entries         uint64 `json:"entries"`
	UniqueInodes    uint64 `json:"inodes"`
	Symlinks        uint64 `json:"symlinks"`
	SameInodeNames  uint32 `json:"same_inode_names,omitempty"`
	RenameSameInode bool   `json:"rename_same_inode,omitempty"`
}

func knownResourceMode(mode Mode) bool {
	switch mode {
	case Normal, CPUBusy, LowCPUSleep, TouchMemory, UntouchedMemory, MultiThreadCPU, FileSmall, FileMulti, FileEntries, FileGrow, FileUnlink, FileEdges, FileHold:
		return true
	default:
		return false
	}
}

func fileMode(mode Mode) bool {
	switch mode {
	case FileSmall, FileMulti, FileEntries, FileGrow, FileUnlink, FileEdges, FileHold:
		return true
	default:
		return false
	}
}
