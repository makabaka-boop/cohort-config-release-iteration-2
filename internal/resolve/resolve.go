// Package resolve 实现客户端配置解析：
//
//   - 发布带 0～100% 试用比例与最低客户端版本；
//   - 按客户端 ID 的稳定哈希把客户端分到 0..99 桶；
//   - 桶落在比例内且版本达标 → 拿到当前代次的包；
//   - 其余客户端（含版本不达标者）回退到“最近的兼容包”，
//     即当前代次之前、最低版本要求不高于客户端版本的最新一代；
//   - 响应同时给出实际包号(package)与发布代次(gen)，
//     调用方能区分自己拿到的是当前灰度还是兼容回退。
package resolve

import (
	"context"
	"errors"

	"configrelease/internal/config"
	"configrelease/internal/hash"
	"configrelease/internal/semver"
	"configrelease/internal/store"
)

// Decision 是一次解析的结果。
type Decision struct {
	// Gen 是实际服务于该客户端的发布代次（可能是历史代次）。
	Gen int64
	// Package 是该代次指向的完整不可变包号。
	Package int64
	// Snapshot 是该包的 routes + limits 完整快照。
	Snapshot config.Snapshot
	// CurrentGen 是系统此刻的当前发布代次。
	CurrentGen int64
	// Trial 表示客户端是否在本次灰度集合内。
	Trial bool
	// Fallback 表示客户端拿到的是兼容回退包而非当前包。
	Fallback bool
	// Bucket 是客户端的稳定桶号 0..99（便于核对分组）。
	Bucket uint64
	// MinClientVersion 是所服务代次声明的最低客户端版本。
	MinClientVersion semver.Version
}

// ErrNoCompatiblePackage 存在发布记录，但没有任何代次兼容该客户端。
var ErrNoCompatiblePackage = errors.New("no compatible package for client version")

// Service 依赖 store 做同事务快照读取。
type Service struct {
	store *store.Store
}

// New 构造解析服务。
func New(st *store.Store) *Service { return &Service{store: st} }

// Resolve 执行解析。
// 返回 store.ErrNoCurrent 表示系统尚未发布；
// 返回 ErrNoCompatiblePackage 表示发布过但该客户端无兼容包。
func (s *Service) Resolve(ctx context.Context, clientID string, clientVer semver.Version) (Decision, error) {
	current, err := s.store.Current(ctx)
	if err != nil {
		return Decision{}, err
	}

	bucket := hash.Bucket(clientID)
	eligible := semver.GTE(clientVer, current.MinVersion)
	selected := eligible && hash.InTrial(bucket, current.TrialPercent)

	d := Decision{CurrentGen: current.Gen, Bucket: bucket}

	if selected {
		// 灰度命中：当前代次 + 当前完整包。
		d.Gen = current.Gen
		d.Package = current.Package
		d.Snapshot = current.Content
		d.Trial = true
		d.MinClientVersion = current.MinVersion
		return d, nil
	}

	// 未命中灰度，或版本不兼容：回退到最近的兼容包（跳过同包历史代次）。
	fb, err := s.store.LatestCompatibleBefore(ctx, clientVer, current.Gen, current.Package)
	if err != nil {
		if errors.Is(err, store.ErrNoCurrent) {
			return Decision{}, ErrNoCompatiblePackage
		}
		return Decision{}, err
	}
	d.Gen = fb.Gen
	d.Package = fb.Package
	d.Snapshot = fb.Content
	d.Fallback = true
	d.MinClientVersion = fb.MinVersion
	return d, nil
}
