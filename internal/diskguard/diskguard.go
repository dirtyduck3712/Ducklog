// Package diskguard 監控磁碟使用率,把「磁碟滿寫壞 catalog」這個最常見死法擋掉。
package diskguard

import "syscall"

type State int

const (
	OK State = iota
	Warn
	Reject
	Purge
)

func (s State) String() string {
	return [...]string{"ok", "warn", "reject", "purge"}[s]
}

const (
	WarnThreshold   = 0.80
	RejectThreshold = 0.90
	PurgeThreshold  = 0.95
)

type Guard struct {
	path    string
	usageFn func(string) (float64, error)
}

func New(path string, usageFn func(string) (float64, error)) *Guard {
	if usageFn == nil {
		usageFn = DiskUsage
	}
	return &Guard{path: path, usageFn: usageFn}
}

func (g *Guard) State() (State, float64, error) {
	u, err := g.usageFn(g.path)
	if err != nil {
		return OK, 0, err
	}
	switch {
	case u >= PurgeThreshold:
		return Purge, u, nil
	case u >= RejectThreshold:
		return Reject, u, nil
	case u >= WarnThreshold:
		return Warn, u, nil
	default:
		return OK, u, nil
	}
}

// DiskUsage 回傳 path 所在檔案系統的已用比例(0..1)。
func DiskUsage(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	avail := float64(st.Bavail) * float64(st.Bsize)
	if total == 0 {
		return 0, nil
	}
	return 1 - avail/total, nil
}
