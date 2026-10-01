// Package store 是 PostgreSQL 存储层。
//
// 并发控制的关键在 CommitGeneration / ReportFault：
// 先 SELECT ... FOR UPDATE 锁住 rollout_state 单行，再核对 expected_gen 或所见 gen，
// 然后在同一事务里写 generation、推进指针。两个 API 实例同时提交时，
// 后拿到行锁的事务会看到已推进的代次，因预期代次过期而 409。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"configrelease/internal/config"
	"configrelease/internal/hash"
	"configrelease/internal/semver"

	"github.com/lib/pq"
)

// 错误哨兵，API 层据此映射 HTTP 状态码。
var (
	// ErrDraftNotFound 草稿尚未写入。
	ErrDraftNotFound = errors.New("draft not found")
	// ErrNoCurrent 还没有任何成功发布。
	ErrNoCurrent = errors.New("no current generation")
	// ErrConflict 预期代次与库内当前代次不一致（并发提交失败方）。
	ErrConflict = errors.New("generation conflict")
	// ErrPackageNotFound 指定的包不存在。
	ErrPackageNotFound = errors.New("package not found")
	// ErrGenerationNotFound 指定的代次不存在。
	ErrGenerationNotFound = errors.New("generation not found")
	// ErrClientIncompatible 上报版本不满足所见代次的最低客户端版本。
	ErrClientIncompatible = errors.New("client version incompatible with generation")
	// ErrClientNotInTrial 按所见代次的分桶与试用比例复核，客户端未命中试用。
	ErrClientNotInTrial = errors.New("client was not in the generation trial")
)

// Store 包装数据库句柄。
type Store struct {
	db *sql.DB
}

// New 建立存储层。
func New(db *sql.DB) *Store { return &Store{db: db} }

// migrationLockKey 是迁移用的固定咨询锁键，多实例启动时串行化 DDL，
// 避免两个实例并发 CREATE TABLE 触发 pg_class 唯一约束竞争。
const migrationLockKey int64 = 0x4352_4652_4D49_4731 // "CRFMIG1"

// Migrate 执行全部 DDL，可安全重复调用，也可被多个实例并发调用。
func (s *Store) Migrate(ctx context.Context) error {
	// advisory lock 必须固定在同一条连接上持有与释放。
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return err
	}
	defer func() {
		// 用独立 context，避免请求取消后锁滞留到连接回收。
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	_, err = conn.ExecContext(ctx, schema)
	return err
}

// GenerationView 是一条发布记录 + 其指向的完整包快照。
type GenerationView struct {
	Gen            int64           `json:"gen"`
	Kind           string          `json:"kind"`
	Package        int64           `json:"package"`
	TrialPercent   int             `json:"trial_percent"`
	MinVersion     semver.Version  `json:"-"`
	MinVersionText string          `json:"min_client_version"`
	RollbackFrom   *int64          `json:"rollback_from,omitempty"`
	FaultFrom      *int64          `json:"fault_from,omitempty"`
	FaultReports   []FaultReport   `json:"-"`
	CreatedAt      time.Time       `json:"created_at"`
	Content        config.Snapshot `json:"snapshot"`
}

// FaultReport 是某客户端针对其确实命中的试用代次提交的不可变故障记录。
type FaultReport struct {
	ID                int64          `json:"id"`
	Gen               int64          `json:"gen"`
	ClientID          string         `json:"client_id"`
	ClientVersion     semver.Version `json:"-"`
	ClientVersionText string         `json:"client_version"`
	Bucket            int            `json:"bucket"`
	CreatedAt         time.Time      `json:"created_at"`
}

const genColumns = `g.gen, g.pkg, g.kind, g.trial_percent,
    g.min_major, g.min_minor, g.min_patch, g.min_version_text,
    g.rollback_from, g.fault_from, g.created_at, p.content`

func scanGen(row interface {
	Scan(dest ...any) error
}) (GenerationView, error) {
	var v GenerationView
	var rollback sql.NullInt64
	var faultFrom sql.NullInt64
	var content []byte
	if err := row.Scan(
		&v.Gen, &v.Package, &v.Kind, &v.TrialPercent,
		&v.MinVersion.Major, &v.MinVersion.Minor, &v.MinVersion.Patch, &v.MinVersionText,
		&rollback, &faultFrom, &v.CreatedAt, &content,
	); err != nil {
		return GenerationView{}, err
	}
	if rollback.Valid {
		rf := rollback.Int64
		v.RollbackFrom = &rf
	}
	if faultFrom.Valid {
		ff := faultFrom.Int64
		v.FaultFrom = &ff
	}
	if err := json.Unmarshal(content, &v.Content); err != nil {
		return GenerationView{}, fmt.Errorf("decode package %d: %w", v.Package, err)
	}
	return v, nil
}

// PutDraft 覆盖单例草稿（不做业务校验，校验发生在 FreezeDraft）。
func (s *Store) PutDraft(ctx context.Context, snap config.Snapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO drafts (id, content, updated_at)
        VALUES (1, $1::jsonb, now())
        ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content, updated_at = now()`,
		string(raw))
	return err
}

// GetDraft 读取当前草稿。
func (s *Store) GetDraft(ctx context.Context) (config.Snapshot, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM drafts WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return config.Snapshot{}, ErrDraftNotFound
	}
	if err != nil {
		return config.Snapshot{}, err
	}
	var snap config.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return config.Snapshot{}, err
	}
	return snap, nil
}

// Package 是冻结得到的完整不可变包。
type Package struct {
	Pkg       int64
	Content   config.Snapshot
	CreatedAt time.Time
}

// freezeLockKey 串行化“草稿 -> 包”的冻结动作，避免多实例同时冻结
// 同一草稿或交错写入。
const freezeLockKey int64 = 0x4352_4652_4D49_4732 // "CRFMIG2"

// FreezeDraft 把当前草稿冻结成一个新的不可变包。
// 咨询锁串行化并发冻结，保证每次冻结拿到各自独立的新包号。
func (s *Store) FreezeDraft(ctx context.Context) (Package, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Package{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 锁在事务内获取，随事务提交/回滚自动释放。
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", freezeLockKey); err != nil {
		return Package{}, err
	}

	var raw []byte
	err = tx.QueryRowContext(ctx,
		`SELECT content FROM drafts WHERE id = 1 FOR UPDATE`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Package{}, ErrDraftNotFound
	}
	if err != nil {
		return Package{}, err
	}

	// 落库前再解码一次，确保包内容是合法 Snapshot。
	var snap config.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Package{}, err
	}
	// 事务内最终校验：即使 API 校验后草稿刚被别的请求覆盖，
	// 也绝不会把含断裂跨组引用的内容冻结成包。
	if err := snap.Validate(); err != nil {
		return Package{}, err
	}

	var pkg Package
	var content []byte
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO packages (content) VALUES ($1::jsonb)
         RETURNING pkg, content, created_at`, string(raw)).
		Scan(&pkg.Pkg, &content, &pkg.CreatedAt); err != nil {
		return Package{}, err
	}
	pkg.Content = snap

	if err := tx.Commit(); err != nil {
		return Package{}, err
	}
	return pkg, nil
}

// GetPackage 返回某包号对应的完整快照。
func (s *Store) GetPackage(ctx context.Context, pkg int64) (Package, error) {
	var raw []byte
	var out Package
	err := s.db.QueryRowContext(ctx,
		`SELECT pkg, content, created_at FROM packages WHERE pkg = $1`, pkg).
		Scan(&out.Pkg, &raw, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Package{}, ErrPackageNotFound
	}
	if err != nil {
		return Package{}, err
	}
	if err := json.Unmarshal(raw, &out.Content); err != nil {
		return Package{}, err
	}
	return out, nil
}

// Current 返回当前发布代次（连同完整包快照）。
func (s *Store) Current(ctx context.Context) (GenerationView, error) {
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`
        SELECT %s
        FROM rollout_state r
        JOIN generations g ON g.gen = r.current_gen
        JOIN packages p ON p.pkg = g.pkg
        WHERE r.id = 1`, genColumns))
	v, err := scanGen(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerationView{}, ErrNoCurrent
	}
	if err != nil {
		return GenerationView{}, err
	}
	if err := s.attachFaultReports(ctx, &v); err != nil {
		return GenerationView{}, err
	}
	return v, nil
}

// ListGenerations 返回全部发布记录（按代次升序），每条都带完整快照。
// “发布记录”因此与解析、回滚指向同一形态的完整数据。
func (s *Store) ListGenerations(ctx context.Context) ([]GenerationView, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
        SELECT %s
        FROM generations g
        JOIN packages p ON p.pkg = g.pkg
        ORDER BY g.gen ASC`, genColumns))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GenerationView
	for rows.Next() {
		v, err := scanGen(rows)
		if err != nil {
			return nil, err
		}
		if err := s.attachFaultReports(ctx, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetGeneration 返回某一代次及其完整快照。
func (s *Store) GetGeneration(ctx context.Context, gen int64) (GenerationView, error) {
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`
        SELECT %s
        FROM generations g
        JOIN packages p ON p.pkg = g.pkg
        WHERE g.gen = $1`, genColumns), gen)
	v, err := scanGen(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerationView{}, ErrGenerationNotFound
	}
	if err != nil {
		return GenerationView{}, err
	}
	if err := s.attachFaultReports(ctx, &v); err != nil {
		return GenerationView{}, err
	}
	return v, nil
}

const faultReportColumns = `id, gen, client_id,
    client_major, client_minor, client_patch, client_version_text,
    bucket, created_at`

func scanFaultReport(row interface{ Scan(dest ...any) error }) (FaultReport, error) {
	var r FaultReport
	if err := row.Scan(
		&r.ID, &r.Gen, &r.ClientID,
		&r.ClientVersion.Major, &r.ClientVersion.Minor, &r.ClientVersion.Patch,
		&r.ClientVersionText, &r.Bucket, &r.CreatedAt,
	); err != nil {
		return FaultReport{}, err
	}
	return r, nil
}

// ListFaultReports 返回某一代次已复核通过的故障上报，按登记顺序排列。
func (s *Store) ListFaultReports(ctx context.Context, gen int64) ([]FaultReport, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
        SELECT %s
        FROM fault_reports
        WHERE gen = $1
        ORDER BY id ASC`, faultReportColumns), gen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FaultReport
	for rows.Next() {
		r, err := scanFaultReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) attachFaultReports(ctx context.Context, v *GenerationView) error {
	if v.FaultFrom == nil {
		return nil
	}
	reports, err := s.ListFaultReports(ctx, *v.FaultFrom)
	if err != nil {
		return err
	}
	v.FaultReports = reports
	return nil
}

// LatestCompatibleBefore 返回 genBefore 之前（不含）的“回退目标”：
// 包号与 currentPkg 不同、最低客户端版本 <= clientVer 的最新一代；
// 找不到返回 ErrNoCurrent。
//
// 两个跳过条件都不可少：
//   - 只看 min_client_version，不看当时的 trial_percent ——
//     回退给客户端的是一份确定的兼容完整快照，不能再让客户端抽签；
//   - 包号必须不同于当前包 —— 同一新包的 0%/中间灰度代次不是“旧版本”，
//     非试用客户端必须落到上一个真正在服务的包（最近一次不同的包）。
func (s *Store) LatestCompatibleBefore(ctx context.Context, clientVer semver.Version, genBefore, currentPkg int64) (GenerationView, error) {
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`
        SELECT %s
        FROM generations g
        JOIN packages p ON p.pkg = g.pkg
        WHERE g.gen < $1
          AND g.pkg <> $2
          AND (g.min_major, g.min_minor, g.min_patch) <= ($3, $4, $5)
        ORDER BY g.gen DESC
        LIMIT 1`, genColumns),
		genBefore, currentPkg, clientVer.Major, clientVer.Minor, clientVer.Patch)
	v, err := scanGen(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerationView{}, ErrNoCurrent
	}
	return v, err
}

// CommitInput 描述一次代次提交（publish / adjust / rollback / fault_disable 共用）。
type CommitInput struct {
	Kind         string         // publish | adjust | rollback | fault_disable
	Pkg          int64          // 该代次指向的完整包
	TrialPercent int            // 0..100
	MinVersion   semver.Version // 最低客户端版本
	RollbackFrom *int64         // 仅 rollback：回滚来源代次
	FaultFrom    *int64         // 仅 fault_disable：触发停用的故障上报所属代次
	ExpectedGen  int64          // 提交者预期的当前代次；首次发布传 0
}

// CommitGeneration 是唯一能推进发布代次的入口。
func (s *Store) CommitGeneration(ctx context.Context, in CommitInput) (GenerationView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return GenerationView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 1) 包必须真实存在（与 generation 同事务校验，回滚/发布都指向完整快照）。
	var pkgExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT true FROM packages WHERE pkg = $1`, in.Pkg).Scan(&pkgExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GenerationView{}, ErrPackageNotFound
		}
		return GenerationView{}, err
	}

	// 2) 锁定当前指针行（首次发布会插入它）。
	var currentGen sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT current_gen FROM rollout_state WHERE id = 1 FOR UPDATE`).Scan(&currentGen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 首次发布：行锁此时还无行可锁，两个事务可能同时走到这里。
		// 无条件插入单行：只有一个事务能赢，另一个拿到 unique_violation。
		if in.ExpectedGen != 0 {
			return GenerationView{}, ErrConflict
		}
		// 3a) 写入不可变代次记录，让其外键先成立。
		newGen, insErr := s.insertGeneration(ctx, tx, in)
		if insErr != nil {
			return GenerationView{}, insErr
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rollout_state (id, current_gen) VALUES (1, $1)`, newGen); err != nil {
			var pqErr *pq.Error
			if errors.As(err, &pqErr) && pqErr.Code == "23505" {
				return GenerationView{}, ErrConflict
			}
			return GenerationView{}, err
		}
		if err := tx.Commit(); err != nil {
			return GenerationView{}, mapPqError(err)
		}
		return s.GetGeneration(ctx, newGen)
	case err != nil:
		return GenerationView{}, err
	default:
		// 3) 乐观并发检查：预期代次必须与库内一致。
		if currentGen.Int64 != in.ExpectedGen {
			return GenerationView{}, ErrConflict
		}
	}

	// 4) 写入不可变代次记录。
	newGen, err := s.insertGeneration(ctx, tx, in)
	if err != nil {
		return GenerationView{}, err
	}

	// 5) 推进当前指针。
	res, err := tx.ExecContext(ctx,
		`UPDATE rollout_state SET current_gen = $1 WHERE id = 1 AND current_gen = $2`,
		newGen, in.ExpectedGen)
	if err != nil {
		return GenerationView{}, err
	}
	// 行锁已串行化，这里的断言是双保险。
	if n, _ := res.RowsAffected(); n != 1 {
		return GenerationView{}, ErrConflict
	}

	if err := tx.Commit(); err != nil {
		return GenerationView{}, mapPqError(err)
	}

	return s.GetGeneration(ctx, newGen)
}

// insertGeneration 在给定事务内追加一条不可变发布代次记录。
func (s *Store) insertGeneration(ctx context.Context, tx *sql.Tx, in CommitInput) (int64, error) {
	var newGen int64
	err := tx.QueryRowContext(ctx, `
        INSERT INTO generations
            (pkg, kind, trial_percent, min_major, min_minor, min_patch,
             min_version_text, rollback_from, fault_from)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        RETURNING gen`,
		in.Pkg, in.Kind, in.TrialPercent,
		in.MinVersion.Major, in.MinVersion.Minor, in.MinVersion.Patch,
		in.MinVersion.String(), in.RollbackFrom, in.FaultFrom).Scan(&newGen)
	if err != nil {
		return 0, mapPqError(err)
	}
	return newGen, nil
}

// mapPqError 把外键冲突等数据库错误映射成领域错误。
func mapPqError(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch pqErr.Code {
		case "23503": // foreign_key_violation
			switch {
			case pqErr.Constraint == "generations_rollback_from_fkey":
				return ErrGenerationNotFound
			case strings.Contains(pqErr.Constraint, "fault_from"):
				return ErrGenerationNotFound
			default:
				return ErrPackageNotFound
			}
		case "40001", "40P01": // serialization failure / deadlock
			return ErrConflict
		}
	}
	return err
}

// FaultTriggerThreshold 是同一发布代次触发自动停用所需的不同有效客户端数。
const FaultTriggerThreshold = 3

// FaultReportInput 是客户端提交的故障见证。
type FaultReportInput struct {
	Gen       int64
	ClientID  string
	Version   semver.Version
}

// FaultReportResult 返回登记结果；第三个不同客户端上报时 Generation 为新代次。
type FaultReportResult struct {
	Created    bool
	Report     FaultReport
	Count      int
	Threshold  int
	Triggered  bool
	Generation GenerationView
}

// ReportFault 按代次登记一个试用故障客户端。
//
// 整个方法在一个事务里完成：锁当前指针、复核该客户端确实按所见代次的分桶和
// 兼容规则取得试用包、幂等登记见证；达到阈值时在同一事务追加 fault_disable
// 代次并推进当前指针。与人工 publish/adjust/rollback 共用 rollout_state 行锁，
// 因此交错写操作由数据库裁决唯一先后顺序。
func (s *Store) ReportFault(ctx context.Context, in FaultReportInput) (FaultReportResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return FaultReportResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var currentGen int64
	if err := tx.QueryRowContext(ctx,
		`SELECT current_gen FROM rollout_state WHERE id = 1 FOR UPDATE`).
		Scan(&currentGen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FaultReportResult{}, ErrNoCurrent
		}
		return FaultReportResult{}, err
	}
	// 所见代次必须仍是当前代次；旧代次上报只可能重放历史，不能触发处置。
	if currentGen != in.Gen {
		return FaultReportResult{}, ErrConflict
	}

	var (
		pkg                         int64
		trialPercent                int
		major, minorV, patch        int64
	)
	err = tx.QueryRowContext(ctx, `
        SELECT pkg, trial_percent, min_major, min_minor, min_patch
        FROM generations
        WHERE gen = $1`, in.Gen).Scan(&pkg, &trialPercent, &major, &minorV, &patch)
	if errors.Is(err, sql.ErrNoRows) {
		return FaultReportResult{}, ErrGenerationNotFound
	}
	if err != nil {
		return FaultReportResult{}, err
	}
	observedMin := semver.Version{Major: major, Minor: minorV, Patch: patch}

	// 服务端必须用该代次自己的规则复核，不能相信客户端声称“我拿到了试用包”。
	if !semver.GTE(in.Version, observedMin) {
		return FaultReportResult{}, ErrClientIncompatible
	}
	bucket := int(hash.Bucket(in.ClientID))
	if !hash.InTrial(uint64(bucket), trialPercent) {
		return FaultReportResult{}, ErrClientNotInTrial
	}

	report := FaultReport{
		Gen:               in.Gen,
		ClientID:          in.ClientID,
		ClientVersion:     in.Version,
		ClientVersionText: in.Version.String(),
		Bucket:            bucket,
	}
	insertErr := tx.QueryRowContext(ctx, `
        INSERT INTO fault_reports
            (gen, client_id, client_major, client_minor, client_patch,
             client_version_text, bucket)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (gen, client_id) DO NOTHING
        RETURNING id, created_at`,
		in.Gen, in.ClientID,
		in.Version.Major, in.Version.Minor, in.Version.Patch,
		in.Version.String(), bucket).
		Scan(&report.ID, &report.CreatedAt)
	if errors.Is(insertErr, sql.ErrNoRows) {
		// 同一客户端对同一代次重复上报只计一次；不产生新记录，也绝不再触发处置。
		existing, err := faultReportInTx(ctx, tx, in.Gen, in.ClientID)
		if err != nil {
			return FaultReportResult{}, err
		}
		count, err := countFaultReportsInTx(ctx, tx, in.Gen)
		if err != nil {
			return FaultReportResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return FaultReportResult{}, mapPqError(err)
		}
		return FaultReportResult{
			Created:   false,
			Report:    existing,
			Count:     count,
			Threshold: FaultTriggerThreshold,
		}, nil
	}
	if insertErr != nil {
		return FaultReportResult{}, mapPqError(insertErr)
	}

	count, err := countFaultReportsInTx(ctx, tx, in.Gen)
	if err != nil {
		return FaultReportResult{}, err
	}
	result := FaultReportResult{
		Created:   true,
		Report:    report,
		Count:     count,
		Threshold: FaultTriggerThreshold,
	}

	if count == FaultTriggerThreshold {
		// 第三个不同有效客户端：故障记录与新代次必须同事务提交。
		faultFrom := in.Gen
		newGen, err := s.insertGeneration(ctx, tx, CommitInput{
			Kind:         "fault_disable",
			Pkg:          pkg,
			TrialPercent: 0,
			MinVersion:   observedMin,
			FaultFrom:    &faultFrom,
			ExpectedGen:  currentGen,
		})
		if err != nil {
			return FaultReportResult{}, err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE rollout_state SET current_gen = $1
             WHERE id = 1 AND current_gen = $2`,
			newGen, currentGen)
		if err != nil {
			return FaultReportResult{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return FaultReportResult{}, ErrConflict
		}
		result.Triggered = true
		if err := tx.Commit(); err != nil {
			return FaultReportResult{}, mapPqError(err)
		}
		generation, err := s.GetGeneration(ctx, newGen)
		if err != nil {
			return FaultReportResult{}, err
		}
		result.Generation = generation
		return result, nil
	}

	if err := tx.Commit(); err != nil {
		return FaultReportResult{}, mapPqError(err)
	}
	return result, nil
}

func faultReportInTx(ctx context.Context, tx *sql.Tx, gen int64, clientID string) (FaultReport, error) {
	row := tx.QueryRowContext(ctx, fmt.Sprintf(`
        SELECT %s
        FROM fault_reports
        WHERE gen = $1 AND client_id = $2`, faultReportColumns), gen, clientID)
	r, err := scanFaultReport(row)
	if errors.Is(err, sql.ErrNoRows) {
		return FaultReport{}, sql.ErrNoRows
	}
	return r, err
}

func countFaultReportsInTx(ctx context.Context, tx *sql.Tx, gen int64) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM fault_reports WHERE gen = $1`, gen).
		Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ResetForTest 清空全部业务表并复位序列，仅供 verify 测试使用。
func (s *Store) ResetForTest(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
        TRUNCATE TABLE rollout_state, fault_reports, generations, packages, drafts
        RESTART IDENTITY CASCADE`)
	return err
}
