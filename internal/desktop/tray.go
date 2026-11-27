package desktop

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"image/png"
	"math"

	"absorb-agent/internal/kind"

	"github.com/energye/systray"
	xdraw "golang.org/x/image/draw"
)

// RunTray 阻塞运行托盘（app.startup 里 go 起子协程）。
// Wails v2 无内置托盘，用 systray 提供最小菜单：显示主窗口 / 退出。
// showMain / quitApp 由调用方注入：恢复主窗口走 ShowMainWindow（同步清理
// TOOLWINDOW 样式使任务栏按钮回归），退出经 QuitApp 置位放行标志后真正退出。
func RunTray(appIconPNG []byte, showMain func(), quitApp func()) {
	systray.Run(func() { onTrayReady(appIconPNG, showMain, quitApp) }, nil)
}

func onTrayReady(appIconPNG []byte, showMain func(), quitApp func()) {
	systray.SetIcon(trayIconBytes(appIconPNG))
	systray.SetTooltip(kind.MainWindowTitle)
	// 左键单击直接打开主窗口；未设 OnRClick，右键保持默认弹出菜单
	systray.SetOnClick(func(systray.IMenu) { showMain() })
	// energye 的菜单项用 Click 回调（无 getlantern 的 ClickedCh channel）
	systray.AddMenuItem("显示主窗口", "打开管理界面").Click(showMain)
	// 先停 systray 循环（移除托盘图标）再走 QuitApp（置位放行标志 + 标准退出）
	systray.AddMenuItem("退出", "停止服务并退出").Click(func() {
		systray.Quit()
		quitApp()
	})
}

// trayIconBytes 将应用图标等比缩放至 32x32、透明底居中，包装为 ICO 容器
// （Windows systray 接受 PNG-in-ICO）。
func trayIconBytes(appIconPNG []byte) []byte {
	const size = 32
	src, err := png.Decode(bytes.NewReader(appIconPNG))
	if err != nil {
		return nil
	}
	sb := src.Bounds()
	scale := math.Min(float64(size)/float64(sb.Dx()), float64(size)/float64(sb.Dy()))
	w := int(float64(sb.Dx())*scale + 0.5)
	h := int(float64(sb.Dy())*scale + 0.5)
	scaled := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, sb, xdraw.Over, nil)
	// 非方形源图居中到正方形画布
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, image.Rect((size-w)/2, (size-h)/2, (size+w)/2, (size+h)/2),
		scaled, image.Pt(0, 0), draw.Src)

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return nil
	}

	// ICO 容器：ICONDIR(6B) + ICONDIRENTRY(16B) + PNG 数据
	var ico bytes.Buffer
	ico.Write([]byte{0, 0, 1, 0, 1, 0}) // 1 张图
	entry := struct {
		W, H, Colors, Reserved uint8
		Planes, BitCount       uint16
		BytesInRes, Offset     uint32
	}{W: size, H: size, Planes: 1, BitCount: 32,
		BytesInRes: uint32(pngBuf.Len()), Offset: 22}
	binary.Write(&ico, binary.LittleEndian, entry)
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}
