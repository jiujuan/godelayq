package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
)

// ImportOnce 把 store.path 指向的 JSON 文件一次性导入任务库（TASK-M03）。
// 三条件齐备才动手：未打标记 && jobs 表为空 && 文件存在——缺任何一条都跳过。
// 打过标记后 JSON 永不再读；文件本身不删除不重命名，归档是运维动作，
// 进程不代劳。导入在单事务里完成，失败即失败，不留半迁状态。
//
// 与 mgr.ImportOnce 同一手法的任务侧版本：mgr 需要三个分项标记是因为它有
// 三个独立来源；任务库只有一个来源，一个标记就够。
func ImportOnce(h *Handle, jsonPath string) error {
	imported, err := alreadyImported(h)
	if err != nil {
		return err
	}
	if imported {
		return nil
	}

	empty, err := tableEmpty(h)
	if err != nil {
		return err
	}
	if !empty {
		// 库里有数据却没有标记：只可能是有人动了库。拒绝重复导入是唯一安全的
		// 反应——导入是给"全新库"用的，不是对账工具。
		return markImported(h)
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 没有历史文件可导：打上标记，空库就是正态。
			return markImported(h)
		}
		return fmt.Errorf("jobs: read import source %s: %w", jsonPath, err)
	}
	if len(data) == 0 {
		return markImported(h)
	}

	var snapshots map[string]core.JobSnapshot
	if err := json.Unmarshal(data, &snapshots); err != nil {
		// 解析失败的 JSON 多半是人手改坏或拷错了文件：拦住启动并给出自救路径，
		// 与 executor profile 文件的口径一致。
		return fmt.Errorf("jobs: parse import source %s: %w; "+
			"fix that file or delete it to start with an empty job store", jsonPath, err)
	}

	tx, err := h.SQL().Begin()
	if err != nil {
		return fmt.Errorf("jobs: import begin: %w", err)
	}
	for id, snap := range snapshots {
		payload, marshalErr := json.Marshal(snap)
		if marshalErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("jobs: marshal import %s: %w", id, marshalErr)
		}
		if _, execErr := tx.Exec(`INSERT INTO jobs (id, name, status, updated_at, snapshot_json)
VALUES (?, ?, ?, ?, ?)`,
			snap.ID, snap.Name, core.JobStatus(snap.Status).String(),
			snap.UpdatedAt.UnixNano(), string(payload)); execErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("jobs: insert import %s: %w", id, execErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("jobs: import commit: %w", err)
	}
	return markImported(h)
}

// ImportOnce 是装配方视角的入口：拿着 Store 就能做导入，不必碰句柄。
func (s *Store) ImportOnce(jsonPath string) error {
	return ImportOnce(s.h, jsonPath)
}

// alreadyImported 报告标记是否已写入。
func alreadyImported(h *Handle) (bool, error) {
	var value string
	err := h.SQL().QueryRow(`SELECT value FROM jobs_meta WHERE key = ?`, metaJobsImported).Scan(&value)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("jobs: read import marker: %w", err)
}

// tableEmpty 报告 jobs 表是否没有行。
func tableEmpty(h *Handle) (bool, error) {
	var count int
	if err := h.SQL().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil {
		return false, fmt.Errorf("jobs: count jobs: %w", err)
	}
	return count == 0, nil
}

// markImported 写标记。值只是排障参考（导入时刻的 Unix 秒）。
func markImported(h *Handle) error {
	_, err := h.SQL().Exec(`INSERT INTO jobs_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaJobsImported, strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		return fmt.Errorf("jobs: write import marker: %w", err)
	}
	return nil
}
