package gui

import (
    "context"
    "fmt"
    "fyne.io/fyne/v2"
    "fyne.io/fyne/v2/app"
    "fyne.io/fyne/v2/container"
    "fyne.io/fyne/v2/dialog"
    "fyne.io/fyne/v2/theme"
    "fyne.io/fyne/v2/widget"
    "github.com/getlantern/systray"
    "WebdavSync/db"
    "WebdavSync/engine"
    "WebdavSync/models"
)

// Run 启动 GUI 和系统托盘
func Run(eng *engine.SyncEngine, db *db.DB) {
    // 初始化 Fyne 应用
    a := app.NewWithID("com.webdavsync")
    w := a.NewWindow("WebDAV Sync")
    w.Resize(fyne.NewSize(800, 600))

    // 设置图标
    w.SetIcon(theme.FileImageIcon())

    // 主界面组件
    statusLabel := widget.NewLabel("状态：空闲")
    logText := widget.NewMultiLineEntry()
    logText.SetPlaceHolder("同步日志...")
    logText.Disable()

    // 配置按钮
    configBtn := widget.NewButton("配置", func() {
        showConfigDialog(w, eng, db)
    })

    // 暂停/恢复按钮，优先定义
    pauseBtn := widget.NewButton("暂停同步", func() {
        if eng.IsPaused() {
            eng.Resume()
            pauseBtn.SetText("暂停同步")
            statusLabel.SetText("状态：运行中")
        } else {
            eng.Pause()
            pauseBtn.SetText("恢复同步")
            statusLabel.SetText("状态：已暂停")
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

    // 系统托盘
    setupTray(a, eng, w, statusLabel, pauseBtn)

    // 启动同步引擎
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go func() {
        if err := eng.Start(ctx); err != nil {
            dialog.ShowError(err, w)
        }
    }()

    // 处理冲突
    go func() {
        for conflict := range eng.Conflicts() {
            showConflictDialog(w, conflict, logText)
        }
    }()

    // 显示窗口
    w.ShowAndRun()
}

// setupTray 设置系统托盘
func setupTray(a fyne.App, eng *engine.SyncEngine, w fyne.Window, statusLabel *widget.Label, pauseBtn *widget.Button) {
    systray.Run(func() {
        systray.SetIcon(getTrayIcon())
        systray.SetTitle("WebDAV Sync")
        systray.SetTooltip("WebDAV 同步工具")
        mShow := systray.AddMenuItem("显示", "显示主窗口")
        mPause := systray.AddMenuItem("暂停同步", "暂停或恢复同步")
        mQuit := systray.AddMenuItem("退出", "退出应用")

        go func() {
            for {
                select {
                case <-mShow.ClickedCh:
                    w.Show()
                case <-mPause.ClickedCh:
                    if eng.IsPaused() {
                        eng.Resume()
                        mPause.SetTitle("暂停同步")
                        statusLabel.SetText("状态：运行中")
                        pauseBtn.SetText("暂停同步")
                    } else {
                        eng.Pause()
                        mPause.SetTitle("恢复同步")
                        statusLabel.SetText("状态：已暂停")
                        pauseBtn.SetText("恢复同步")
                    }
                case <-mQuit.ClickedCh:
                    systray.Quit()
                    a.Quit()
                }
            }
        }()
    }, nil)
}

// getTrayIcon 返回托盘图标数据
func getTrayIcon() []byte {
    return []byte{}
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
    modeSelect := widget.NewSelect([]string{"bidirectional", "source-to-target", "target-to-source"}, func(s string) {
        cfg.Mode = s
    })
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
            if err := models.Save(db.DB, cfg); err != nil {
                dialog.ShowError(err, w)
                return
            }
            eng.UpdateConfig(cfg)
            dialog.ShowInformation("成功", "配置已保存", w)
        },
    }
    dialog.ShowForm("配置同步", "保存", "取消", form.Items, func(ok bool) {
        if ok {
            form.OnSubmit()
        }
    }, w)
}

// showConflictDialog 显示冲突解决对话框
func showConflictDialog(w fyne.Window, conflict models.Conflict, logText *widget.Entry) {
    localBtn := widget.NewButton("保留本地", func() {
        conflict.Choice <- "local"
        logText.SetText(logText.Text + fmt.Sprintf("\n冲突解决：%s 保留本地", conflict.File.Path))
    })
    remoteBtn := widget.NewButton("保留云端", func() {
        conflict.Choice <- "remote"
        logText.SetText(logText.Text + fmt.Sprintf("\n冲突解决：%s 保留云端", conflict.File.Path))
    })
    ignoreBtn := widget.NewButton("忽略", func() {
        conflict.Choice <- "ignore"
        logText.SetText(logText.Text + fmt.Sprintf("\n冲突忽略：%s", conflict.File.Path))
    })

    content := container.NewVBox(
        widget.NewLabel(fmt.Sprintf("文件冲突：%s", conflict.File.Path)),
        widget.NewLabel("请选择解决方式："),
        localBtn,
        remoteBtn,
        ignoreBtn,
    )

    dialog.ShowCustom("解决冲突", "关闭", content, w)
}