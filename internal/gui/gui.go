package gui

// Run 启动 Wails GUI 主窗口（阻塞直到窗口关闭）。

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

// Run 创建主窗口并运行事件循环。窗口关闭后由前端调用过 Shutdown，
// 同步运行时已收尾；这里只负责窗口生命周期。
func Run() error {
	b := NewBridge()
	return wails.Run(&options.App{
		Title:     "Onerclone",
		Width:     420,
		Height:    640,
		MinWidth:  360,
		MinHeight: 480,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 243, G: 243, B: 243, A: 1},
		OnStartup:        func(ctx StartupContext) { b.startup(ctx) },
		OnShutdown:       func(ctx StartupContext) { b.shutdown() },
		Bind: []interface{}{
			b,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
}
