package main

// tray.go —— Phase 2：系统托盘常驻（Q8/Q14 的“托盘”半边）。
//
// 菜单：打开面板（一次性链接，随启动刷新）/ 打开同步根 / 打开日志 / 退出。
// 托盘只是壳：所有动作都转给已有能力（面板 URL 是全局的，退出走 trayQuit
// 与 Ctrl+C 同一条退出路径）。托盘失败（如无交互桌面）不影响同步本身。

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

	mPanel := systray.AddMenuItem("打开面板", "打开本地 Web 面板")
	mRoot := systray.AddMenuItem("打开同步根", "在资源管理器中打开")
	mLog := systray.AddMenuItem("打开日志", "用记事本查看 onerclone.log")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止同步并退出")

	go func() {
		for {
			select {
			case <-mPanel.ClickedCh:
				u, _ := panelURL.Load().(string)
				if u == "" {
					log.Print("面板链接尚未就绪")
					continue
				}
				openInBrowser(u)
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
				select { // 与 Ctrl+C 同一条退出路径
				case trayQuit <- struct{}{}:
				default:
				}
			}
		}
	}()
}

// openInBrowser 用系统默认浏览器打开链接。
func openInBrowser(u string) {
	// rundll32 url.dll,FileProtocolHandler 是 Windows 上最稳的“交给默认浏览器”方式
	if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u).Start(); err != nil {
		log.Printf("打开浏览器失败: %v", err)
	}
}
