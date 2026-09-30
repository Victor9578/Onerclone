package cfapi

import "testing"

func TestNewPoliciesTracksDirectories(t *testing.T) {
	if got := NewPolicies().InSync; got != InSyncPolicyTrackAll {
		t.Fatalf("InSync policy = %#x, want %#x", got, InSyncPolicyTrackAll)
	}
}

func TestPlaceholderCreateFlagsForDirectories(t *testing.T) {
	const mark = PlaceholderCreateFlagMarkInSync
	const disablePopulation = PlaceholderCreateFlagDisableOnDemandPopulation

	// 目录：去掉 MARK_IN_SYNC（绿勾由引擎在动作结算后统一补标），
	// 加 DISABLE_ON_DEMAND_POPULATION。
	if got := placeholderCreateFlags(true, mark); got != disablePopulation {
		t.Fatalf("directory flags = %#x, want %#x", got, disablePopulation)
	}
	if got := placeholderCreateFlags(true, 0); got != disablePopulation {
		t.Fatalf("default directory flags = %#x, want %#x", got, disablePopulation)
	}
	// 文件：调用方给的 flags 原样保留（含 MARK_IN_SYNC）。
	if got := placeholderCreateFlags(false, mark); got != mark {
		t.Fatalf("file flags = %#x, want %#x", got, mark)
	}
	if got := placeholderCreateFlags(false, 0); got != 0 {
		t.Fatalf("default file flags = %#x, want 0", got)
	}
}
