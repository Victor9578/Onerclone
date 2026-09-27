package main

// autostart.go —— 开机自启（HKCU\...\Run，普通用户权限，无需 UAC）。
//
// 用法：
//   onerclone autostart -enable    写入自启（指向当前 exe）
//   onerclone autostart -disable   移除自启
//   onerclone autostart            查询当前状态
//
// 安装包（installer.iss）会在安装时按用户勾选自动调用 enable。

import (
	"flag"
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	autostartKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartName   = "Onerclone"
)

// cmdAutostart 管理开机自启。
func cmdAutostart(args []string) {
	fs := flag.NewFlagSet("autostart", flag.ExitOnError)
	enable := fs.Bool("enable", false, "开启开机自启")
	disable := fs.Bool("disable", false, "关闭开机自启")
	exe := fs.String("exe", "", "自启指向的 exe（默认当前 exe）")
	_ = fs.Parse(args)

	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartKey,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		log.Fatalf("打开自启注册表项失败: %v", err)
	}
	defer key.Close()

	switch {
	case *disable:
		if err := key.DeleteValue(autostartName); err != nil {
			if err == registry.ErrNotExist {
				log.Print("开机自启本来就是关闭的")
				return
			}
			log.Fatalf("关闭自启失败: %v", err)
		}
		log.Print("✅ 已关闭开机自启")

	case *enable:
		p := *exe
		if p == "" {
			p, _ = os.Executable()
		}
		if p == "" {
			log.Fatal("无法确定 exe 路径，请用 -exe 指定")
		}
		// 带引号，路径含空格也安全
		if err := key.SetStringValue(autostartName, `"`+p+`"`); err != nil {
			log.Fatalf("写入自启失败: %v", err)
		}
		log.Printf("✅ 已开启开机自启: %s", p)

	default:
		v, _, err := key.GetStringValue(autostartName)
		if err == registry.ErrNotExist {
			fmt.Println("开机自启: 关闭")
			return
		}
		if err != nil {
			log.Fatalf("查询自启失败: %v", err)
		}
		fmt.Println("开机自启: 开启")
		fmt.Printf("指向: %s\n", v)
	}
}
