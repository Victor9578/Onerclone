package app

// tray.go —— Phase 2：系统托盘常驻（Q8/Q14 的“托盘”半边）。
//
// 菜单：打开同步根 / 打开日志 / 退出。网页面板从产品路径移除，登录走 CLI。
// 托盘只是壳：所有动作都转给已有能力（退出走 trayQuit，与 Ctrl+C 同一条
// 退出路径）。托盘失败（如无交互桌面）不影响同步本身。

import (
	_ "embed"
	"log"
	"os/exec"
	"sync/atomic"

	"github.com/getlantern/systray"
)

//go:embed ui/tray.ico
var trayIcon []byte

var (
	// trayRoot/trayLog 由 cmdRun 填充（托盘菜单要用）。
	trayRoot atomic.Value // string
	trayLog  atomic.Value // string
)

// startTray 异步启动托盘；panic/失败只记日志，不影响同步。
func startTray() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("⚠ 托盘启动失败（同步不受影响）: %v", r)
			}
		}()
		systray.Run(onTrayReady, func() {
			log.Print("托盘已退出")
		})
	}()
}

func onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("Onerclone")
	systray.SetTooltip("Onerclone —— 夸克网盘 Windows 原生同步")

	mRoot := systray.AddMenuItem("打开同步根", "在资源管理器中打开")
	mLog := systray.AddMenuItem("打开日志", "用记事本查看 onerclone.log")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止同步并退出")

	go func() {
		for {
			select {
			case <-mRoot.ClickedCh:
				r, _ := trayRoot.Load().(string)
				if r == "" {
					continue
				}
				if err := exec.Command("explorer.exe", r).Start(); err != nil {
					log.Printf("打开同步根失败: %v", err)
				}
			case <-mLog.ClickedCh:
				l, _ := trayLog.Load().(string)
				if l == "" {
					continue
				}
				if err := exec.Command("notepad.exe", l).Start(); err != nil {
					log.Printf("打开日志失败: %v", err)
				}
			case <-mQuit.ClickedCh:
				log.Print("托盘菜单：退出")
				systray.Quit()
				select { // 与 Ctrl+C 同一条退出路径；缓冲 1 保证不丢
				case trayQuit <- struct{}{}:
				default:
				}
			}
		}
	}()
}
