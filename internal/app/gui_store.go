package app

// gui_store.go —— GUI 侧对状态库的窄接口 + RetryAllFailed 实现。
// 不直接导出 *state.Store：GUI 只需要这几个读操作，窄接口防误用。

import "onerclone/internal/state"

// StoreView 是 GUI 需要的状态库面（*state.Store 满足它）。
type StoreView interface {
	Stats() (map[state.ActionState]int, error)
	CountAuthFailed() (int, error)
	ListActions(states []state.ActionState, limit int) ([]state.Action, error)
	Retry(id int64) error
}

// retryAllFailed 把所有 failed 动作重置为 pending（分类退避保留，
// 下个调度周期自动重跑）。返回重置条数。
func retryAllFailed(v StoreView) (int, error) {
	acts, err := v.ListActions([]state.ActionState{state.StateFailed}, 1000)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range acts {
		if err := v.Retry(a.ID); err == nil {
			n++
		}
	}
	return n, nil
}
