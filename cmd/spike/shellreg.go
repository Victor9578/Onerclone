package main

// shellreg.go —— Shell 集成注册（SyncRootManager 注册表项）。
//
// 根因记录（2026-09-25 图标验收）：CfRegisterSyncRoot 只做内核/驱动级注册，
// **不会**写 HKLM\...\Explorer\SyncRootManager 注册表项。Explorer 的云朵/
// 绿勾状态图标完全依赖这层 Shell 注册（官方文档 "Integrate a Cloud
// Storage Provider" 明确该键由 provider 自己创建）。缺它 = 文件系统状态
// 全对但图标永远不显示。
//
// 键名格式：[ProviderName]![Windows SID]![AccountID]
// 需要的值：DisplayNameResource / IconResource / Flags /
//           UserSyncRoots\[SID] = 同步根路径

import (
	"fmt"
	"log"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const srmBase = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager`

// currentSID 返回当前用户的 SID 字符串（S-1-5-...）。
func currentSID() (string, error) {
	tok := windows.GetCurrentProcessToken()
	var need uint32
	// TokenUser = 1；两段式调用拿所需缓冲大小
	_ = windows.GetTokenInformation(tok, windows.TokenUser, nil, 0, &need)
	if need == 0 {
		return "", fmt.Errorf("GetTokenInformation 尺寸探测失败")
	}
	buf := make([]byte, need)
	if err := windows.GetTokenInformation(tok, windows.TokenUser, &buf[0], need, &need); err != nil {
		return "", fmt.Errorf("GetTokenInformation 失败: %w", err)
	}
	// TOKEN_USER { SID_AND_ATTRIBUTES User; } —— 首字段即 SID 指针
	tu := (*windows.SIDAndAttributes)(unsafe.Pointer(&buf[0]))
	return tu.Sid.String(), nil
}

// shellRegKey 返回 SyncRootManager 下本 provider 的键名。
func shellRegKey() (string, error) {
	sid, err := currentSID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`%s\%s!%s!%s`, srmBase, providerName, sid, syncRootID), nil
}

// shellRegister 写入 SyncRootManager 注册表项，让 Explorer 识别同步根并
// 显示状态图标（绿勾/云朵）。优先写 HKLM（普通用户通常可写，实测
// elevated=false 成功）；被拒时降级写 HKCU（OneDrive 也用 HKCU 层，
// Explorer 两层都认）。
func shellRegister(syncRoot string) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	key := fmt.Sprintf(`%s\%s!%s!%s`, srmBase, providerName, sid, syncRootID)

	root, _, err := registry.CreateKey(registry.LOCAL_MACHINE, key,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		// HKLM 写不进去（某些机器普通用户无权限）→ 降级 HKCU
		root, _, err = registry.CreateKey(registry.CURRENT_USER, key,
			registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			return fmt.Errorf("创建 SyncRootManager 键失败（HKLM 与 HKCU 均被拒）: %w", err)
		}
		log.Printf("⚠ HKLM 不可写，已降级写 HKCU SyncRootManager（Explorer 同样识别）")
	}
	k := root
	defer k.Close()

	if err := k.SetStringValue("", providerName); err != nil {
		return fmt.Errorf("写默认值失败: %w", err)
	}
	if err := k.SetStringValue("DisplayNameResource", providerName); err != nil {
		return fmt.Errorf("写 DisplayNameResource 失败: %w", err)
	}
	// 云服务图标（imageres.dll -1043 = 云朵）；Phase 2 换成自己的图标资源
	if err := k.SetExpandStringValue("IconResource",
		`%SystemRoot%\System32\imageres.dll,-1043`); err != nil {
		return fmt.Errorf("写 IconResource 失败: %w", err)
	}
	// 0x162 = OneDrive 同款 Flags（状态图标启用相关），照抄实测有效
	if err := k.SetDWordValue("Flags", 0x162); err != nil {
		return fmt.Errorf("写 Flags 失败: %w", err)
	}

	uk, _, err := registry.CreateKey(k, `UserSyncRoots`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("创建 UserSyncRoots 失败: %w", err)
	}
	defer uk.Close()
	if err := uk.SetStringValue(sid, syncRoot); err != nil {
		return fmt.Errorf("写 UserSyncRoots 路径失败: %w", err)
	}
	return nil
}

// shellUnregister 删除 SyncRootManager 注册表项（注销同步根时调用）。
// HKLM 与 HKCU 两层都尝试清理（shellRegister 可能降级写过 HKCU）。
func shellUnregister() error {
	key, err := shellRegKey()
	if err != nil {
		return err
	}
	// 先删子键 UserSyncRoots，再删主键（registry.DeleteKey 不递归）
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		if err := registry.DeleteKey(root, key+`\UserSyncRoots`); err != nil &&
			err != registry.ErrNotExist {
			return fmt.Errorf("删除 UserSyncRoots 失败: %w", err)
		}
		if err := registry.DeleteKey(root, key); err != nil &&
			err != registry.ErrNotExist {
			return fmt.Errorf("删除 SyncRootManager 键失败: %w", err)
		}
	}
	return nil
}
