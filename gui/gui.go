package gui

import (
	"context"
	"fmt"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"WebdavSync/db"
	"WebdavSync/engine"
	"WebdavSync/models"
)

// Run 启动 GUI 和系统托盘
func Run(eng *engine.SyncEngine, db *db.DB) {
	a := app.NewWithID("com.webdavsync")
	w := a.NewWindow("WebDAV Sync")
	w.Resize(fyne.NewSize(800, 600))
	w.SetIcon(theme.FileImageIcon())

	// 主界面组件
	statusLabel := widget.NewLabel("状态：空闲")
	logText := widget.NewMultiLineEntry()
	logText.SetPlaceHolder("同步日志...")
	logText.Disable()

	configBtn := widget.NewButton("配置", func() {
		showConfigDialog(w, eng, db)
	})

	var pauseBtn *widget.Button
	pauseBtn = widget.NewButton("暂停同步", func() {
		if eng.IsPaused() {
			eng.Resume()
			pauseBtn.SetText("暂停同步")
			statusLabel.SetText("状态：运行中")
			updateTrayMenu(a, eng, w, statusLabel, pauseBtn)
			logText.SetText(logText.Text + "\n同步已恢复")
		} else {
			eng.Pause()
			pauseBtn.SetText("恢复同步")
			statusLabel.SetText("状态：已暂停")
			updateTrayMenu(a, eng, w, statusLabel, pauseBtn)
			logText.SetText(logText.Text + "\n同步已暂停")
		}
	})

	// 主布局
	content := container.NewVBox(
		statusLabel,
		configBtn,
		pauseBtn,
		widget.NewLabel("同步日志："),
		container.NewVScroll(logText),
	)
	w.SetContent(content)

	// 设置系统托盘
	setupTray(a, eng, w, statusLabel, pauseBtn)

	// 启动同步引擎
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := eng.Start(ctx); err != nil {
			dialog.ShowError(err, w)
			logText.SetText(logText.Text + "\n启动同步引擎失败: " + err.Error())
		} else {
			logText.SetText(logText.Text + "\n同步引擎已启动")
		}
	}()

	// 处理冲突
	go func() {
		for conflict := range eng.Conflicts() {
			showConflictDialog(w, conflict, logText)
		}
	}()

	w.ShowAndRun()
}

// setupTray 设置系统托盘 (使用 Fyne 内置实现)
func setupTray(a fyne.App, eng *engine.SyncEngine, w fyne.Window, statusLabel *widget.Label, pauseBtn *widget.Button) {
	if desk, ok := a.(desktop.App); ok {
		m := fyne.NewMenu("WebDAV Sync",
			fyne.NewMenuItem("显示主窗口", func() {
				w.Show()
			}),
			fyne.NewMenuItem("暂停/恢复同步", func() {
				if eng.IsPaused() {
					eng.Resume()
					pauseBtn.SetText("暂停同步")
					statusLabel.SetText("状态：运行中")
				} else {
					eng.Pause()
					pauseBtn.SetText("恢复同步")
					statusLabel.SetText("状态：已暂停")
				}
				updateTrayMenu(a, eng, w, statusLabel, pauseBtn)
			}),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("退出", func() {
				a.Quit()
			}),
		)
		desk.SetSystemTrayMenu(m)
		desk.SetSystemTrayIcon(theme.FyneLogo())
	}
}

// updateTrayMenu 更新托盘菜单状态
func updateTrayMenu(a fyne.App, eng *engine.SyncEngine, w fyne.Window, statusLabel *widget.Label, pauseBtn *widget.Button) {
	if desk, ok := a.(desktop.App); ok {
		var pauseText string
		if eng.IsPaused() {
			pauseText = "恢复同步"
		} else {
			pauseText = "暂停同步"
		}
		
		m := fyne.NewMenu("WebDAV Sync",
			fyne.NewMenuItem("显示主窗口", func() {
				w.Show()
			}),
			fyne.NewMenuItem(pauseText, func() {
				if eng.IsPaused() {
					eng.Resume()
					pauseBtn.SetText("暂停同步")
					statusLabel.SetText("状态：运行中")
				} else {
					eng.Pause()
					pauseBtn.SetText("恢复同步")
					statusLabel.SetText("状态：已暂停")
				}
				updateTrayMenu(a, eng, w, statusLabel, pauseBtn)
			}),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("退出", func() {
				a.Quit()
			}),
		)
		desk.SetSystemTrayMenu(m)
	}
}

// showConfigDialog 显示配置对话框
func showConfigDialog(w fyne.Window, eng *engine.SyncEngine, db *db.DB) {
	cfg, err := models.Load(db.DB)
	if err != nil {
		dialog.ShowError(err, w)
		return
	}

	urlEntry := widget.NewEntry()
	urlEntry.SetText(cfg.URL)
	userEntry := widget.NewEntry()
	userEntry.SetText(cfg.User)
	passEntry := widget.NewPasswordEntry()
	passEntry.SetText(cfg.Pass)
	localDirEntry := widget.NewEntry()
	localDirEntry.SetText(cfg.LocalDir)
	remoteDirEntry := widget.NewEntry()
	remoteDirEntry.SetText(cfg.RemoteDir)
	modeSelect := widget.NewSelect([]string{"bidirectional", "source-to-target", "target-to-source"}, func(s string) {})
	modeSelect.SetSelected(cfg.Mode)

	form := &widget.Form{
		Items: []*widget.FormItem{
			{Text: "WebDAV URL", Widget: urlEntry},
			{Text: "用户名", Widget: userEntry},
			{Text: "密码", Widget: passEntry},
			{Text: "本地目录", Widget: localDirEntry},
			{Text: "云端目录", Widget: remoteDirEntry},
			{Text: "同步模式", Widget: modeSelect},
		},
		OnSubmit: func() {
			cfg.URL = urlEntry.Text
			cfg.User = userEntry.Text
			cfg.Pass = passEntry.Text
			cfg.LocalDir = localDirEntry.Text
			cfg.RemoteDir = remoteDirEntry.Text
			cfg.Mode = modeSelect.Selected
			if err := models.Save(db.DB, cfg); err != nil {
				dialog.ShowError(err, w)
				return
			}
			eng.UpdateConfig(cfg)
			dialog.ShowInformation("成功", "配置已保存", w)
		},
	}

	dialog.ShowCustomConfirm("配置同步", "保存", "取消", 
		container.NewVScroll(form), 
		func(ok bool) {
			if ok {
				form.OnSubmit()
			}
		}, w)
}

// showConflictDialog 显示冲突解决对话框
func showConflictDialog(w fyne.Window, conflict models.Conflict, logText *widget.Entry) {
	dialog.ShowCustomConfirm("解决冲突",
		"",
		"",
		container.NewVBox(
			widget.NewLabel(fmt.Sprintf("文件冲突: %s", conflict.File.Path)),
			widget.NewLabel("请选择解决方式:"),
			container.NewHBox(
				widget.NewButton("保留本地", func() {
					conflict.Choice <- "local"
					logText.SetText(logText.Text + fmt.Sprintf("\n冲突解决: %s 保留本地", conflict.File.Path))
				}),
				widget.NewButton("保留云端", func() {
					conflict.Choice <- "remote"
					logText.SetText(logText.Text + fmt.Sprintf("\n冲突解决: %s 保留云端", conflict.File.Path))
				}),
				widget.NewButton("忽略", func() {
					conflict.Choice <- "ignore"
					logText.SetText(logText.Text + fmt.Sprintf("\n冲突忽略: %s", conflict.File.Path))
				}),
			),
		),
		func(bool) {}, w)
}