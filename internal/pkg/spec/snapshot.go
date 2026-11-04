package spec

// 项目设置里快照间隔和份数上限留空（0）时的默认值，见 docs/snapshot.md。
const (
	DefaultSnapshotIntervalMinutes = 720
	DefaultSnapshotLimit           = 10
)

// SnapshotIntervalMinutes 返回生效的定时快照间隔（分钟）。
func SnapshotIntervalMinutes(n uint32) uint32 {
	if n == 0 {
		return DefaultSnapshotIntervalMinutes
	}
	return n
}

// SnapshotLimit 返回生效的快照份数上限。
func SnapshotLimit(n uint32) uint32 {
	if n == 0 {
		return DefaultSnapshotLimit
	}
	return n
}
