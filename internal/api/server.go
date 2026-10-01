// Package api 组装 HTTP 路由。所有写操作都显式携带 expected_gen，
// 由存储层的行锁 + 代次比较保证并发安全。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"configrelease/internal/config"
	"configrelease/internal/resolve"
	"configrelease/internal/store"
)

// Server 持有处理器依赖。
type Server struct {
	store   *store.Store
	resolve *resolve.Service
	mux     *http.ServeMux
}

// NewServer 创建 API 服务器。
func NewServer(st *store.Store, rs *resolve.Service) *Server {
	s := &Server{store: st, resolve: rs, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP 使 Server 直接成为 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /healthz", s.healthz)

	// 草稿：可随意读写，冻结前不影响任何已发布内容。
	m.HandleFunc("GET /v1/draft", s.getDraft)
	m.HandleFunc("PUT /v1/draft", s.putDraft)
	m.HandleFunc("POST /v1/draft/validate", s.validateDraft)

	// 不可变包：只能由“冻结草稿”产生，可按包号读取。
	m.HandleFunc("POST /v1/packages", s.freezePackage)
	m.HandleFunc("GET /v1/packages/{pkg}", s.getPackage)

	// 发布代次：三种提交都带 expected_gen。
	m.HandleFunc("POST /v1/publish", s.publish)
	m.HandleFunc("POST /v1/adjust", s.adjust)
	m.HandleFunc("POST /v1/rollback", s.rollback)

	m.HandleFunc("GET /v1/current", s.current)
	m.HandleFunc("GET /v1/generations", s.listGenerations)
	m.HandleFunc("GET /v1/generations/{gen}", s.getGeneration)

	// 客户端解析：稳定哈希分组 + 旧版本回退。
	m.HandleFunc("GET /v1/resolve", s.resolveConfig)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.GetDraft(r.Context())
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
}

func (s *Server) putDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Snapshot config.Snapshot `json:"snapshot"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.store.PutDraft(r.Context(), body.Snapshot); err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": body.Snapshot})
}

// validateDraft 做跨组引用等校验，不修改任何状态。
// 空 body 表示校验当前已保存的草稿；带 {"snapshot": ...} 则校验给定内容。
func (s *Server) validateDraft(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeAppError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	var snap *config.Snapshot
	if len(bytes.TrimSpace(raw)) > 0 {
		var body struct {
			Snapshot *config.Snapshot `json:"snapshot"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeAppError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		snap = body.Snapshot
	}

	if snap == nil {
		stored, err := s.store.GetDraft(r.Context())
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		snap = &stored
	}
	if err := snap.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) freezePackage(w http.ResponseWriter, r *http.Request) {
	// 先做跨组引用校验，通过后才冻结成不可变包。
	snap, err := s.store.GetDraft(r.Context())
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := snap.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	pkg, err := s.store.FreezeDraft(r.Context())
	if err != nil {
		if _, ok := config.AsValidationError(err); ok {
			writeValidationError(w, err)
			return
		}
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"package":    pkg.Pkg,
		"snapshot":   pkg.Content,
		"created_at": pkg.CreatedAt,
	})
}

func (s *Server) getPackage(w http.ResponseWriter, r *http.Request) {
	pkg, ok := pathInt64(w, r, "pkg")
	if !ok {
		return
	}
	p, err := s.store.GetPackage(r.Context(), pkg)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"package":    p.Pkg,
		"snapshot":   p.Content,
		"created_at": p.CreatedAt,
	})
}

type publishRequest struct {
	Package          int64  `json:"package"`
	TrialPercent     *int   `json:"trial_percent"`
	MinClientVersion string `json:"min_client_version"`
	ExpectedGen      int64  `json:"expected_gen"`
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if !decodeBody(w, r, &req) {
		return
	}

	ve := &validationErrs{}
	if req.Package <= 0 {
		ve.add("package", "must be > 0")
	}
	// 发布必须显式声明最低客户端版本。
	minVer, verErr := parseRequiredVersion(req.MinClientVersion)
	if verErr != "" {
		ve.add("min_client_version", verErr)
	}
	// 试用比例必填且必须落在 0～100。
	if req.TrialPercent == nil {
		ve.add("trial_percent", "is required and must be between 0 and 100")
	} else if *req.TrialPercent < 0 || *req.TrialPercent > 100 {
		ve.add("trial_percent", "must be between 0 and 100")
	}
	if !ve.flush(w) {
		return
	}
	percent := *req.TrialPercent

	gen, err := s.store.CommitGeneration(r.Context(), store.CommitInput{
		Kind:         "publish",
		Pkg:          req.Package,
		TrialPercent: percent,
		MinVersion:   minVer,
		ExpectedGen:  req.ExpectedGen,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, genResponse(gen))
}

func (s *Server) adjust(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TrialPercent     *int   `json:"trial_percent"`
		MinClientVersion string `json:"min_client_version"`
		ExpectedGen      int64  `json:"expected_gen"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	// 调整必须针对已有的当前代次；包保持不变，因此是新代次而非就地修改。
	cur, err := s.store.Current(r.Context())
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	ve := &validationErrs{}
	if body.TrialPercent == nil && body.MinClientVersion == "" {
		ve.add("body", "provide trial_percent and/or min_client_version")
	}
	percent := cur.TrialPercent
	if body.TrialPercent != nil {
		if *body.TrialPercent < 0 || *body.TrialPercent > 100 {
			ve.add("trial_percent", "must be between 0 and 100")
		} else {
			percent = *body.TrialPercent
		}
	}
	minVer := cur.MinVersion
	if body.MinClientVersion != "" {
		v, err := parseSemVer(body.MinClientVersion)
		if err != "" {
			ve.add("min_client_version", err)
		} else {
			minVer = v
		}
	}
	if !ve.flush(w) {
		return
	}

	gen, err := s.store.CommitGeneration(r.Context(), store.CommitInput{
		Kind:         "adjust",
		Pkg:          cur.Package,
		TrialPercent: percent,
		MinVersion:   minVer,
		ExpectedGen:  body.ExpectedGen,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, genResponse(gen))
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Gen          int64 `json:"gen"`
		TrialPercent *int  `json:"trial_percent"`
		ExpectedGen  int64 `json:"expected_gen"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	if body.Gen <= 0 {
		writeAppError(w, http.StatusUnprocessableEntity, "invalid_field",
			"gen must reference an existing generation")
		return
	}
	if body.TrialPercent != nil && (*body.TrialPercent < 0 || *body.TrialPercent > 100) {
		writeAppError(w, http.StatusUnprocessableEntity, "invalid_field",
			"trial_percent must be between 0 and 100")
		return
	}

	// 回滚目标：某个历史代次记录，连同它指向的完整包快照一起取回。
	target, err := s.store.GetGeneration(r.Context(), body.Gen)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	// 缺省按 100% 全量回到该快照；最低版本沿用目标代次声明，
	// 保证“查询、发布记录、回滚指向同一完整快照”。
	percent := 100
	if body.TrialPercent != nil {
		percent = *body.TrialPercent
	}
	from := body.Gen

	gen, err := s.store.CommitGeneration(r.Context(), store.CommitInput{
		Kind:         "rollback",
		Pkg:          target.Package,
		TrialPercent: percent,
		MinVersion:   target.MinVersion,
		RollbackFrom: &from,
		ExpectedGen:  body.ExpectedGen,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, genResponse(gen))
}

func (s *Server) current(w http.ResponseWriter, r *http.Request) {
	cur, err := s.store.Current(r.Context())
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, genResponse(cur))
}

func (s *Server) listGenerations(w http.ResponseWriter, r *http.Request) {
	gens, err := s.store.ListGenerations(r.Context())
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(gens))
	for _, g := range gens {
		out = append(out, genResponse(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"generations": out})
}

func (s *Server) getGeneration(w http.ResponseWriter, r *http.Request) {
	gen, ok := pathInt64(w, r, "gen")
	if !ok {
		return
	}
	g, err := s.store.GetGeneration(r.Context(), gen)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, genResponse(g))
}

func (s *Server) resolveConfig(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	verText := q.Get("client_version")
	if clientID == "" || verText == "" {
		writeAppError(w, http.StatusBadRequest, "missing_parameter",
			"client_id and client_version are required")
		return
	}
	ver, msg := parseSemVer(verText)
	if msg != "" {
		writeAppError(w, http.StatusUnprocessableEntity, "invalid_client_version", msg)
		return
	}

	d, err := s.resolve.Resolve(r.Context(), clientID, ver)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoCurrent):
			writeAppError(w, http.StatusServiceUnavailable, "no_current",
				"no configuration has been published yet")
		case errors.Is(err, resolve.ErrNoCompatiblePackage):
			writeAppError(w, http.StatusNotFound, "no_compatible_package",
				"no published package is compatible with this client version")
		default:
			writeStoreError(w, r, err)
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		// 实际服务于该客户端的包号与发布代次，二者成对出现。
		"gen":                d.Gen,
		"package":            d.Package,
		"current_gen":        d.CurrentGen,
		"trial":              d.Trial,
		"fallback":           d.Fallback,
		"bucket":             d.Bucket,
		"min_client_version": d.MinClientVersion.String(),
		"snapshot":           d.Snapshot,
	})
}
