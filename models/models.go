package models

import (
    "database/sql"
)

// Config 存储同步配置
type Config struct {
    URL       string // WebDAV URL
    User      string // 用户名
    Pass      string // 密码
    LocalDir  string // 本地同步目录
    RemoteDir string // 云端同步目录
    Mode      string // 同步模式：bidirectional, source-to-target, target-to-source
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
    return Config{
        URL:       "",
        User:      "",
        Pass:      "",
        LocalDir:  "",
        RemoteDir: "",
        Mode:      "bidirectional",
    }
}

// Load 从数据库加载配置
func Load(db *sql.DB) (Config, error) {
    cfg := DefaultConfig()
    rows, err := db.Query("SELECT key, value FROM config")
    if err != nil {
        return cfg, err
    }
    defer rows.Close()

    for rows.Next() {
        var key, value string
        if err := rows.Scan(&key, &value); err != nil {
            return cfg, err
        }
        switch key {
        case "url":
            cfg.URL = value
        case "user":
            cfg.User = value
        case "pass":
            cfg.Pass = value
        case "local_dir":
            cfg.LocalDir = value
        case "remote_dir":
            cfg.RemoteDir = value
        case "mode":
            cfg.Mode = value
        }
    }
    return cfg, nil
}

// Save 保存配置到数据库
func Save(db *sql.DB, cfg Config) error {
    tx, err := db.Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()

    upsert := `INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`
    _, err = tx.Exec(upsert, "url", cfg.URL)
    if err != nil {
        return err
    }
    _, err = tx.Exec(upsert, "user", cfg.User)
    if err != nil {
        return err
    }
    _, err = tx.Exec(upsert, "pass", cfg.Pass)
    if err != nil {
        return err
    }
    _, err = tx.Exec(upsert, "local_dir", cfg.LocalDir)
    if err != nil {
        return err
    }
    _, err = tx.Exec(upsert, "remote_dir", cfg.RemoteDir)
    if err != nil {
        return err
    }
    _, err = tx.Exec(upsert, "mode", cfg.Mode)
    if err != nil {
        return err
    }

    return tx.Commit()
}

// FileInfo 存储文件同步信息
type FileInfo struct {
    Path        string // 文件路径（相对于同步目录）
    LocalHash   string // 本地文件哈希
    RemoteHash  string // 云端文件哈希
    LocalMtime  int64  // 本地修改时间（Unix 时间戳）
    RemoteMtime int64  // 云端修改时间（Unix 时间戳）
    LastSync    int64  // 最后同步时间（Unix 时间戳）
    Status      string // 状态：synced, local_modified, remote_modified, local_deleted, remote_deleted
}

// Task 存储同步任务
type Task struct {
    ID          int64  // 任务 ID
    Path        string // 文件路径
    Operation   string // 操作：upload, download, delete_local, delete_remote
    Status      string // 状态：pending, completed, failed
    Retries     int    // 重试次数
    LastAttempt int64  // 最后尝试时间（Unix 时间戳）
    ChunkOffset int64  // 分片上传偏移量
}

// Conflict 表示文件冲突
type Conflict struct {
    File   FileInfo
    Choice chan string // 解决方式：local, remote, ignore
}