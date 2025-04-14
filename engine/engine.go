package engine

import (
    "context"
    "crypto/sha1"
    "database/sql"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "time"

    "github.com/fsnotify/fsnotify"
    "github.com/rs/zerolog"
    "github.com/studio-b12/gowebdav"
    "WebdavSync/models"
)

type SyncEngine struct {
    client           *gowebdav.Client
    localDir         string
    remoteDir        string
    mode             string
    conflicts        chan models.Conflict
    taskQueue        chan models.Task
    logger           zerolog.Logger
    db               *sql.DB
    networkAvailable bool
    config           models.Config
    paused           bool
}

func NewSyncEngine(cfg models.Config, db *sql.DB) *SyncEngine {
    client := gowebdav.NewClient(cfg.URL, cfg.User, cfg.Pass)
    logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
    engine := &SyncEngine{
        client:           client,
        localDir:         cfg.LocalDir,
        remoteDir:        cfg.RemoteDir,
        mode:             cfg.Mode,
        conflicts:        make(chan models.Conflict),
        taskQueue:        make(chan models.Task, 100),
        logger:           logger,
        db:               db,
        networkAvailable: true,
        config:           cfg,
        paused:           false,
    }
    go engine.monitorNetwork()
    go engine.retryTasks()
    return engine
}

func (se *SyncEngine) Conflicts() <-chan models.Conflict {
    return se.conflicts
}

func (se *SyncEngine) Pause() {
    se.paused = true
    se.logger.Info().Msg("同步已暂停")
}

func (se *SyncEngine) Resume() {
    se.paused = false
    se.logger.Info().Msg("同步已恢复")
    se.resumeTasks()
}

func (se *SyncEngine) IsPaused() bool {
    return se.paused
}

func (se *SyncEngine) UpdateConfig(cfg models.Config) {
    se.config = cfg
    se.client = gowebdav.NewClient(cfg.URL, cfg.User, cfg.Pass)
    se.localDir = cfg.LocalDir
    se.remoteDir = cfg.RemoteDir
    se.mode = cfg.Mode
    se.logger.Info().Msg("同步配置已更新")
}

func (se *SyncEngine) monitorNetwork() {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    for range ticker.C {
        wasAvailable := se.networkAvailable
        se.networkAvailable = se.checkNetwork()
        if !wasAvailable && se.networkAvailable {
            se.logger.Info().Msg("网络恢复，同步已恢复")
            se.resumeTasks()
        } else if wasAvailable && !se.networkAvailable {
            se.logger.Info().Msg("网络断开，正在缓存变更")
        }
    }
}

func (se *SyncEngine) checkNetwork() bool {
    client := &http.Client{Timeout: 3 * time.Second}
    _, err := client.Get(se.config.URL)
    return err == nil
}

func (se *SyncEngine) Start(ctx context.Context) error {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        se.logger.Error().Err(err).Msg("启动文件监控失败")
        return err
    }
    defer watcher.Close()

    go func() {
        for {
            select {
            case event, ok := <-watcher.Events:
                if !ok {
                    return
                }
                if se.paused {
                    continue
                }
                se.handleLocalChange(event)
            case err, ok := <-watcher.Errors:
                if !ok {
                    return
                }
                se.logger.Error().Err(err).Msg("文件监控错误")
            }
        }
    }()

    err = watcher.Add(se.localDir)
    if err != nil {
        se.logger.Error().Err(err).Msg("添加监控目录失败")
        return err
    }

    go se.pollRemote(ctx)
    return nil
}

func (se *SyncEngine) handleLocalChange(event fsnotify.Event) {
    relPath, _ := filepath.Rel(se.localDir, event.Name)
    file := models.FileInfo{Path: relPath}

    if event.Op&fsnotify.Remove == fsnotify.Remove {
        file.Status = "local_deleted"
        file.LocalMtime = 0
        file.LocalHash = ""
        se.logger.Info().Msgf("本地文件 %s 已删除", file.Path)
        _, err := se.db.Exec("INSERT OR REPLACE INTO files (path, status) VALUES (?, ?)", file.Path, file.Status)
        if err != nil {
            se.logger.Error().Err(err).Msg("保存文件状态失败")
        }
        if se.networkAvailable {
            se.compareAndSync(file)
        } else {
            se.queueTask(models.Task{Path: file.Path, Operation: "delete_remote", Status: "pending"})
        }
        return
    }

    if f, err := os.Open(event.Name); err == nil {
        h := sha1.New()
        io.Copy(h, f)
        file.LocalHash = fmt.Sprintf("%x", h.Sum(nil))
        fi, _ := f.Stat()
        file.LocalMtime = fi.ModTime().Unix()
        file.Status = "local_modified"
        f.Close()
        se.logger.Info().Msgf("本地文件 %s 已修改", file.Path)
        _, err = se.db.Exec("INSERT OR REPLACE INTO files (path, local_hash, local_mtime, status) VALUES (?, ?, ?, ?)",
            file.Path, file.LocalHash, file.LocalMtime, file.Status)
        if err != nil {
            se.logger.Error().Err(err).Msg("保存文件状态失败")
        }
        if se.networkAvailable {
            se.compareAndSync(file)
        } else {
            se.queueTask(models.Task{Path: file.Path, Operation: "upload", Status: "pending"})
        }
    }
}

func (se *SyncEngine) pollRemote(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if !se.networkAvailable || se.paused {
                continue
            }
            remoteFiles, err := se.client.ReadDir(se.remoteDir)
            if err != nil {
                se.logger.Error().Err(err).Msg("轮询云端失败")
                se.networkAvailable = false
                continue
            }

            localFiles, err := se.getLocalFilesFromDB()
            if err != nil {
                se.logger.Error().Err(err).Msg("获取文件列表失败")
                continue
            }

            for _, lf := range localFiles {
                found := false
                for _, rf := range remoteFiles {
                    relPath := rf.Name() // 使用 Name() 获取文件名
                    if lf.Path == relPath {
                        found = true
                        lf.RemoteMtime = rf.ModTime().Unix()
                        if lf.RemoteMtime > lf.LastSync {
                            lf.Status = "remote_modified"
                            se.logger.Info().Msgf("云端文件 %s 已修改", lf.Path)
                            _, err = se.db.Exec("UPDATE files SET remote_mtime = ?, status = ? WHERE path = ?",
                                lf.RemoteMtime, lf.Status, lf.Path)
                            if err != nil {
                                se.logger.Error().Err(err).Msg("更新文件状态失败")
                            }
                            se.compareAndSync(lf)
                        }
                        break
                    }
                }
                if !found && lf.Status != "remote_deleted" && lf.RemoteMtime > 0 {
                    lf.RemoteHash = ""
                    lf.RemoteMtime = 0
                    lf.Status = "remote_deleted"
                    se.logger.Info().Msgf("云端文件 %s 已删除", lf.Path)
                    _, err = se.db.Exec("UPDATE files SET remote_mtime = ?, status = ? WHERE path = ?",
                        lf.RemoteMtime, lf.Status, lf.Path)
                    if err != nil {
                        se.logger.Error().Err(err).Msg("更新文件状态失败")
                    }
                    se.compareAndSync(lf)
                }
            }
        }
    }
}

func (se *SyncEngine) compareAndSync(file models.FileInfo) {
    dbFile, err := se.getFileFromDB(file.Path)
    if err != nil {
        se.logger.Error().Err(err).Msgf("获取文件 %s 失败", file.Path)
        return
    }
    lastSync := dbFile.LastSync

    if (dbFile.Status == "local_deleted" && dbFile.RemoteMtime > lastSync) ||
        (dbFile.Status == "remote_deleted" && dbFile.LocalMtime > lastSync) ||
        (dbFile.Status == "local_modified" && dbFile.RemoteMtime > lastSync) {
        choice := make(chan string)
        se.conflicts <- models.Conflict{File: dbFile, Choice: choice}
        switch <-choice {
        case "local":
            if dbFile.Status == "local_deleted" {
                se.queueTask(models.Task{Path: dbFile.Path, Operation: "delete_remote", Status: "pending"})
            } else {
                se.queueTask(models.Task{Path: dbFile.Path, Operation: "upload", Status: "pending"})
            }
            se.logger.Info().Msgf("冲突解决：%s 保留本地", dbFile.Path)
        case "remote":
            if dbFile.Status == "remote_deleted" {
                se.queueTask(models.Task{Path: dbFile.Path, Operation: "delete_local", Status: "pending"})
            } else {
                se.queueTask(models.Task{Path: dbFile.Path, Operation: "download", Status: "pending"})
            }
            se.logger.Info().Msgf("冲突解决：%s 保留云端", dbFile.Path)
        case "ignore":
            se.logger.Info().Msgf("冲突忽略：%s 保持现状", dbFile.Path)
        }
        return
    }

    switch dbFile.Status {
    case "local_deleted":
        if se.mode != "target-to-source" {
            se.queueTask(models.Task{Path: dbFile.Path, Operation: "delete_remote", Status: "pending"})
        }
    case "remote_deleted":
        if se.mode != "source-to-target" {
            se.queueTask(models.Task{Path: dbFile.Path, Operation: "delete_local", Status: "pending"})
        }
    case "local_modified":
        if se.mode != "target-to-source" {
            se.queueTask(models.Task{Path: dbFile.Path, Operation: "upload", Status: "pending"})
        }
    case "remote_modified":
        if se.mode != "source-to-target" {
            se.queueTask(models.Task{Path: dbFile.Path, Operation: "download", Status: "pending"})
        }
    }
}

func (se *SyncEngine) queueTask(task models.Task) {
    task.LastAttempt = time.Now().Unix()
    _, err := se.db.Exec("INSERT OR REPLACE INTO tasks (path, operation, status, retries, last_attempt, chunk_offset) VALUES (?, ?, ?, ?, ?, ?)",
        task.Path, task.Operation, task.Status, task.Retries, task.LastAttempt, task.ChunkOffset)
    if err != nil {
        se.logger.Error().Err(err).Msg("保存任务失败")
    }
    se.taskQueue <- task
    se.logger.Info().Msgf("任务已缓存：%s %s", task.Operation, task.Path)
}

func (se *SyncEngine) retryTasks() {
    for task := range se.taskQueue {
        if !se.networkAvailable || task.Retries >= 5 || se.paused {
            time.Sleep(time.Second << uint(task.Retries))
            task.Retries++
            _, err := se.db.Exec("UPDATE tasks SET retries = ?, last_attempt = ? WHERE path = ? AND operation = ?",
                task.Retries, time.Now().Unix(), task.Path, task.Operation)
            if err != nil {
                se.logger.Error().Err(err).Msg("更新任务失败")
            }
            se.taskQueue <- task
            continue
        }
        if err := se.executeTask(task); err != nil {
            se.logger.Error().Err(err).Msgf("任务失败：%s %s", task.Operation, task.Path)
            task.Retries++
            _, err = se.db.Exec("UPDATE tasks SET retries = ?, last_attempt = ?, status = 'failed' WHERE path = ? AND operation = ?",
                task.Retries, time.Now().Unix(), task.Path, task.Operation)
            if err != nil {
                se.logger.Error().Err(err).Msg("更新任务失败")
            }
            se.taskQueue <- task
        } else {
            _, err = se.db.Exec("UPDATE tasks SET status = 'completed', last_attempt = ? WHERE path = ? AND operation = ?",
                time.Now().Unix(), task.Path, task.Operation)
            if err != nil {
                se.logger.Error().Err(err).Msg("更新任务失败")
            }
            se.logger.Info().Msgf("任务完成：%s %s", task.Operation, task.Path)
        }
    }
}

func (se *SyncEngine) executeTask(task models.Task) error {
    file, err := se.getFileFromDB(task.Path)
    if err != nil {
        return err
    }
    switch task.Operation {
    case "upload":
        return se.uploadWithResume(file, task)
    case "download":
        return se.download(file)
    case "delete_remote":
        return se.deleteRemote(file)
    case "delete_local":
        return se.deleteLocal(file)
    }
    return fmt.Errorf("未知任务：%s", task.Operation)
}

func (se *SyncEngine) resumeTasks() {
    tasks, err := se.getPendingTasksFromDB()
    if err != nil {
        se.logger.Error().Err(err).Msg("恢复任务失败")
        return
    }
    for _, task := range tasks {
        se.taskQueue <- task
        se.logger.Info().Msgf("恢复任务：%s %s", task.Operation, task.Path)
    }
}

func (se *SyncEngine) uploadWithResume(file models.FileInfo, task models.Task) error {
    localPath := filepath.Join(se.localDir, file.Path)
    remotePath := filepath.Join(se.remoteDir, file.Path)
    f, err := os.Open(localPath)
    if err != nil {
        return err
    }
    defer f.Close()
    f.Seek(task.ChunkOffset, 0)
    err = se.client.WriteStream(remotePath, f, 0644)
    if err != nil {
        return err
    }
    file.Status = "synced"
    file.LastSync = time.Now().Unix()
    _, err = se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
        file.Status, file.LastSync, file.Path)
    if err != nil {
        se.logger.Error().Err(err).Msg("更新文件状态失败")
    }
    return nil
}

func (se *SyncEngine) download(file models.FileInfo) error {
    localPath := filepath.Join(se.localDir, file.Path)
    remotePath := filepath.Join(se.remoteDir, file.Path)
    data, err := se.client.ReadStream(remotePath)
    if err != nil {
        return err
    }
    defer data.Close()
    f, err := os.Create(localPath)
    if err != nil {
        return err
    }
    defer f.Close()
    io.Copy(f, data)
    file.Status = "synced"
    file.LastSync = time.Now().Unix()
    _, err = se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
        file.Status, file.LastSync, file.Path)
    if err != nil {
        se.logger.Error().Err(err).Msg("更新文件状态失败")
    }
    return nil
}

func (se *SyncEngine) deleteRemote(file models.FileInfo) error {
    remotePath := filepath.Join(se.remoteDir, file.Path)
    err := se.client.Remove(remotePath)
    if err != nil {
        return err
    }
    file.Status = "synced"
    file.LastSync = time.Now().Unix()
    _, err = se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
        file.Status, file.LastSync, file.Path)
    if err != nil {
        se.logger.Error().Err(err).Msg("更新文件状态失败")
    }
    return nil
}

func (se *SyncEngine) deleteLocal(file models.FileInfo) error {
    localPath := filepath.Join(se.localDir, file.Path)
    err := os.Remove(localPath)
    if err != nil {
        return err
    }
    file.Status = "synced"
    file.LastSync = time.Now().Unix()
    _, err = se.db.Exec("UPDATE files SET status = ?, last_sync = ? WHERE path = ?",
        file.Status, file.LastSync, file.Path)
    if err != nil {
        se.logger.Error().Err(err).Msg("更新文件状态失败")
    }
    return nil
}

func (se *SyncEngine) getFileFromDB(path string) (models.FileInfo, error) {
    var file models.FileInfo
    row := se.db.QueryRow("SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status FROM files WHERE path = ?", path)
    err := row.Scan(&file.Path, &file.LocalHash, &file.RemoteHash, &file.LocalMtime, &file.RemoteMtime, &file.LastSync, &file.Status)
    return file, err
}

func (se *SyncEngine) getLocalFilesFromDB() ([]models.FileInfo, error) {
    rows, err := se.db.Query("SELECT path, local_hash, remote_hash, local_mtime, remote_mtime, last_sync, status FROM files")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var files []models.FileInfo
    for rows.Next() {
        var file models.FileInfo
        if err := rows.Scan(&file.Path, &file.LocalHash, &file.RemoteHash, &file.LocalMtime, &file.RemoteMtime, &file.LastSync, &file.Status); err != nil {
            return nil, err
        }
        files = append(files, file)
    }
    return files, nil
}

func (se *SyncEngine) getPendingTasksFromDB() ([]models.Task, error) {
    rows, err := se.db.Query("SELECT id, path, operation, status, retries, last_attempt, chunk_offset FROM tasks WHERE status = 'pending'")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var tasks []models.Task
    for rows.Next() {
        var task models.Task
        if err := rows.Scan(&task.ID, &task.Path, &task.Operation, &task.Status, &task.Retries, &task.LastAttempt, &task.ChunkOffset); err != nil {
            return nil, err
        }
        tasks = append(tasks, task)
    }
    return tasks, nil
}