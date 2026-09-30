package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"godelayq/core"
	"godelayq/executor"
)

// errNeedDirCheck 是对账少了判断函数的情形：没有它就无法判断哪一行的文件已经不在，
// 而"全都标掉"或"全都不标"都是错的方向。
var errNeedDirCheck = errors.New("sqlite: ReconcileMissing needs a directory check")

// ArtifactIndexOptions 是产物索引的参数。
type ArtifactIndexOptions struct {
	// RootDir 是产物根目录（配置里的 executors.output.dir）。绝对路径不入库，
	// 写库前用它把 OutPath/ErrPath 折成相对写法（out_rel / err_rel）。
	RootDir string
	// Now 是时间源，nil 时用 time.Now。写入行上的 created_at 由它给。
	Now func() time.Time
}

// ArtifactIndex 是 artifact_index 表的实现，同时满足 executor.ArtifactIndexer
// 与 executor.IndexReconciler。
//
// 定位是加速器而不是账本（设计文档 D8）：这里每个方法的错误都由调用方记日志，
// 没有一个会改动执行结果，也没有一个会阻止产物文件被读到。
type ArtifactIndex struct {
	db      *DB
	rootDir string
	now     func() time.Time
	logger  *slog.Logger
}

// NewArtifactIndex 构造产物索引。db 为 nil 或根目录为空时返回错误而不是 panic：
// 后者折不出相对路径，静默建出来的索引每一行都要靠猜。
func NewArtifactIndex(db *DB, opts ArtifactIndexOptions, logger *slog.Logger) (*ArtifactIndex, error) {
	if db == nil {
		return nil, errors.New("sqlite: NewArtifactIndex requires an observability database")
	}
	root := strings.TrimSpace(opts.RootDir)
	if root == "" {
		return nil, errors.New("sqlite: NewArtifactIndex requires the artifact root directory")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &ArtifactIndex{db: db, rootDir: root, now: opts.Now, logger: logger}, nil
}

// insertArtifact 是一行的写入语句。冲突时整行覆盖，created_at 跟着更新：
// 同一次尝试被二次登记（清理后重读、重试撞上同一个 attempt）说的是
// "这份文件现在长这样"，留旧时间会让 TTL 相关的判断读到一个已经不存在的事实。
const insertArtifact = `INSERT INTO artifact_index
	(job_id, attempt, kind, profile, out_rel, err_rel, out_bytes, err_bytes, truncated, state, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(job_id, attempt) DO UPDATE SET
		kind = excluded.kind,
		profile = excluded.profile,
		out_rel = excluded.out_rel,
		err_rel = excluded.err_rel,
		out_bytes = excluded.out_bytes,
		err_bytes = excluded.err_bytes,
		truncated = excluded.truncated,
		state = excluded.state,
		created_at = excluded.created_at`

// Record 登记一次执行。写入状态恒为 available：调用点在文件刚关完之后，
// IndexRecord 里的 State 与 CreatedAt 在写侧被忽略（见该结构的字段注释）。
func (i *ArtifactIndex) Record(rec executor.IndexRecord) error {
	if err := executor.CheckArtifactJobID(rec.JobID); err != nil {
		return fmt.Errorf("sqlite: artifact index: %w", err)
	}
	if rec.Attempt < 0 {
		return fmt.Errorf("sqlite: artifact index: attempt must not be negative, got %d", rec.Attempt)
	}

	outRel, err := i.relative(rec.Info.OutPath)
	if err != nil {
		return err
	}
	errRel, err := i.relative(rec.Info.ErrPath)
	if err != nil {
		return err
	}

	truncated := 0
	if rec.Info.Truncated {
		truncated = 1
	}
	_, err = i.db.sqlDB.Exec(insertArtifact,
		rec.JobID, rec.Attempt, rec.Kind, rec.Profile, outRel, errRel,
		rec.Info.OutBytes, rec.Info.ErrBytes, truncated,
		core.ArtifactAvailable, i.now().Unix())
	if err != nil {
		return fmt.Errorf("sqlite: record artifact %s/%d: %w", rec.JobID, rec.Attempt, err)
	}
	return nil
}

// relative 把绝对路径折成相对产物根目录的写法（库里统一用正斜杠）。
// 空路径折成空，因为它就是"这一路没有输出文件"；落在根目录之外的路径直接报错，
// 那种路径进不了对账，也不该被写成一个看起来能用的相对写法。
func (i *ArtifactIndex) relative(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	rel, err := filepath.Rel(i.rootDir, path)
	if err != nil {
		return "", fmt.Errorf("sqlite: cannot relativise %s against %s: %w", path, i.rootDir, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("sqlite: artifact path %s is outside the artifact root %s", path, i.rootDir)
	}
	return filepath.ToSlash(rel), nil
}

// markPurged 把一个或全部尝试标成已清理，返回改动的行数。
//
// 谓词里带上 state = available：SQLite 的 UPDATE 把"匹配到但值没变"的行也算进
// RowsAffected，那样对账每轮都会把同一批行重新计一次数，日志里的数字没有意义。
// 加上这一条之后重复标注既不改动行也不计数，标注因此是幂等的。
func (i *ArtifactIndex) markPurged(jobID string, attempt *int) (int64, error) {
	var (
		res  sql.Result
		err  error
		args []any
	)
	if attempt == nil {
		args = []any{core.ArtifactPurged, jobID, core.ArtifactAvailable}
		res, err = i.db.sqlDB.Exec(
			`UPDATE artifact_index SET state = ? WHERE job_id = ? AND state = ?`, args...)
	} else {
		args = []any{core.ArtifactPurged, jobID, *attempt, core.ArtifactAvailable}
		res, err = i.db.sqlDB.Exec(
			`UPDATE artifact_index SET state = ? WHERE job_id = ? AND attempt = ? AND state = ?`, args...)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: mark artifact purged for %s: %w", jobID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: count marked artifacts for %s: %w", jobID, err)
	}
	return affected, nil
}

// MarkPurged 只标这一次尝试：同一任务其他尝试的产物可能还在，全标是假话。
func (i *ArtifactIndex) MarkPurged(jobID string, attempt int) error {
	if err := executor.CheckArtifactJobID(jobID); err != nil {
		return fmt.Errorf("sqlite: artifact index: %w", err)
	}
	_, err := i.markPurged(jobID, &attempt)
	return err
}

// MarkAllPurged 标掉一个任务的全部行，供启动期对账使用。
func (i *ArtifactIndex) MarkAllPurged(jobID string) error {
	if err := executor.CheckArtifactJobID(jobID); err != nil {
		return fmt.Errorf("sqlite: artifact index: %w", err)
	}
	_, err := i.markPurged(jobID, nil)
	return err
}

// DeleteByJob 删掉一个任务的全部行。调用方必须在产物目录删除成功之后，
// 顺序反过来会留下"索引说没有、文件还在"的状态。
func (i *ArtifactIndex) DeleteByJob(jobID string) error {
	if err := executor.CheckArtifactJobID(jobID); err != nil {
		return fmt.Errorf("sqlite: artifact index: %w", err)
	}
	if _, err := i.db.sqlDB.Exec(`DELETE FROM artifact_index WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("sqlite: delete artifacts for %s: %w", jobID, err)
	}
	return nil
}

// artifactSelect 是读取语句的列序，与 scanArtifact 一一对应。
const artifactSelect = `SELECT job_id, attempt, kind, profile, out_rel, err_rel,
	out_bytes, err_bytes, truncated, state, created_at
	FROM artifact_index`

// List 按 attempt 升序返回一个任务的全部行：重试链的自然阅读顺序。
func (i *ArtifactIndex) List(jobID string) ([]executor.IndexRecord, error) {
	if err := executor.CheckArtifactJobID(jobID); err != nil {
		return nil, fmt.Errorf("sqlite: artifact index: %w", err)
	}
	rows, err := i.db.sqlDB.Query(artifactSelect+` WHERE job_id = ? ORDER BY attempt ASC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list artifacts for %s: %w", jobID, err)
	}
	defer rows.Close()

	// 空列表而不是 nil：端点把它序列化出去是 []，调用方不必再判一次
	out := make([]executor.IndexRecord, 0, 4)
	for rows.Next() {
		rec, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan artifact: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate artifacts: %w", err)
	}
	return out, nil
}

// Exists 判断索引里有没有这一次尝试。
func (i *ArtifactIndex) Exists(jobID string, attempt int) (bool, error) {
	if err := executor.CheckArtifactJobID(jobID); err != nil {
		return false, fmt.Errorf("sqlite: artifact index: %w", err)
	}
	var one int
	err := i.db.sqlDB.QueryRow(
		`SELECT 1 FROM artifact_index WHERE job_id = ? AND attempt = ? LIMIT 1`, jobID, attempt).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: check artifact %s/%d: %w", jobID, attempt, err)
	}
	return true, nil
}

// rowArtifacts 是 *sql.Rows 的最小读面，让行还原能单独被测。
type rowArtifacts interface {
	Scan(dest ...any) error
}

// scanArtifact 还原一行。时间与路径都是"读侧字段"：写侧的 State/CreatedAt/Info.OutPath
// 由实现自己填，读回来放在这里，调用方不会拿它们去开文件。
func scanArtifact(row rowArtifacts) (executor.IndexRecord, error) {
	var (
		jobID      string
		attempt    int
		kind       string
		profile    sql.NullString
		outRel     sql.NullString
		errRel     sql.NullString
		outBytes   int64
		errBytes   int64
		truncated  int
		state      string
		createdSec int64
	)
	err := row.Scan(&jobID, &attempt, &kind, &profile, &outRel, &errRel,
		&outBytes, &errBytes, &truncated, &state, &createdSec)
	if err != nil {
		return executor.IndexRecord{}, err
	}

	rec := executor.IndexRecord{
		JobID:     jobID,
		Attempt:   attempt,
		Kind:      kind,
		Profile:   profile.String,
		State:     state,
		CreatedAt: time.Unix(createdSec, 0),
		OutRel:    outRel.String,
		ErrRel:    errRel.String,
		Info: executor.ArtifactInfo{
			JobID:     jobID,
			Attempt:   attempt,
			OutBytes:  outBytes,
			ErrBytes:  errBytes,
			Truncated: truncated != 0,
		},
	}
	return rec, nil
}

// ReconcileMissing 把"索引里有、目录已经不在"的 available 行标成 purged，返回标注的行数。
//
// 目录是否存在由产物存储判断（它才懂目录布局），这里只负责查行与改状态。
// 只标注、不删除：手工放回文件时行还在，本期不做把 purged 纠正回 available 的反向对账。
func (i *ArtifactIndex) ReconcileMissing(dirExists func(jobID string) bool) (int, error) {
	if dirExists == nil {
		return 0, errNeedDirCheck
	}

	rows, err := i.db.sqlDB.Query(
		`SELECT DISTINCT job_id FROM artifact_index WHERE state = ?`, core.ArtifactAvailable)
	if err != nil {
		return 0, fmt.Errorf("sqlite: list indexed jobs: %w", err)
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			return 0, fmt.Errorf("sqlite: scan indexed job id: %w", err)
		}
		if !dirExists(jobID) {
			missing = append(missing, jobID)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite: iterate indexed jobs: %w", err)
	}

	marked := 0
	for _, jobID := range missing {
		affected, err := i.markPurged(jobID, nil)
		if err != nil {
			return marked, err
		}
		marked += int(affected)
	}
	return marked, nil
}

// 编译期把两个能力钉在类型上：接口定义在 executor 侧（消费方声明能力），
// 装配方在这里确认实现没有漏方法。
var (
	_ executor.ArtifactIndexer = (*ArtifactIndex)(nil)
	_ executor.IndexReconciler = (*ArtifactIndex)(nil)
)
