package main

import (
    "WebdavSync/db"
    "WebdavSync/engine"
    "WebdavSync/gui"
    "WebdavSync/models" // 更新为 models
)

func main() {
    // 初始化数据库
    db, err := db.NewDB("sync.db")
    if err != nil {
        panic(err)
    }
    defer db.Close()

    // 加载配置
    cfg, err := models.Load(db.DB) // 使用 models.Load
    if err != nil {
        cfg = models.DefaultConfig()
    }

    // 初始化同步引擎
    eng := engine.NewSyncEngine(cfg, db.DB)

    // 启动 GUI
    gui.Run(eng, db)
}