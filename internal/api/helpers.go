package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"configrelease/internal/config"
	"configrelease/internal/semver"
	"configrelease/internal/store"
)

// genResponse 是发布代次的统一对外形态：代次元信息 + 完整包快照。
// 任何读到代次的接口都拿到同一结构，杜绝“新路由配旧限额”。
func genResponse(g store.GenerationView) map[string]any {
	out := map[string]any{
		"gen":                g.Gen,
		"kind":               g.Kind,
		"package":            g.Package,
		"trial_percent":      g.TrialPercent,
		"min_client_version": g.MinVersion.String(),
		"created_at":         g.CreatedAt,
		"snapshot":           g.Content,
	}
	// rollback_from 保持既有响应形态；非回滚代次仍返回 null。
	out["rollback_from"] = g.RollbackFrom
	if g.FaultFrom != nil {
		out["fault_from"] = *g.FaultFrom
	}
	if len(g.FaultReports) > 0 {
		out["fault_reports"] = faultReportsResponse(g.FaultReports)
	}
	return out
}

func faultReportResponse(r store.FaultReport) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"gen":            r.Gen,
		"client_id":      r.ClientID,
		"client_version": r.ClientVersion.String(),
		"bucket":         r.Bucket,
		"created_at":     r.CreatedAt,
	}
}

func faultReportsResponse(reports []store.FaultReport) []map[string]any {
	out := make([]map[string]any, 0, len(reports))
	for _, r := range reports {
		out = append(out, faultReportResponse(r))
	}
	return out
}

// validationErrs 累积请求字段错误。
type validationErrs struct {
	fields []config.FieldError
}

func (v *validationErrs) add(field, msg string) {
	v.fields = append(v.fields, config.FieldError{Field: field, Message: msg})
}

// flush 在有错误时写出 422 并返回 false。
func (v *validationErrs) flush(w http.ResponseWriter) bool {
	if len(v.fields) == 0 {
		return true
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"fields": v.fields,
	})
	return false
}

func parseSemVer(s string) (semver.Version, string) {
	v, err := semver.Parse(s)
	if err != nil {
		return semver.Version{}, "must be semantic version like 1.4.2: " + err.Error()
	}
	return v, ""
}

// parseRequiredVersion 用于发布：最低版本必须提供。
func parseRequiredVersion(s string) (semver.Version, string) {
	if strings.TrimSpace(s) == "" {
		return semver.Version{}, "is required on publish"
	}
	return parseSemVer(s)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeAppError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if dec.More() {
		writeAppError(w, http.StatusBadRequest, "invalid_json", "unexpected trailing content")
		return false
	}
	return true
}

func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		writeAppError(w, http.StatusUnprocessableEntity, "invalid_field",
			name+" must be a positive integer")
		return 0, false
	}
	return n, true
}

func writeAppError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": code, "message": msg})
}

func writeValidationError(w http.ResponseWriter, err error) {
	if ve, ok := config.AsValidationError(err); ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": ve.Fields,
		})
		return
	}
	writeAppError(w, http.StatusInternalServerError, "internal", err.Error())
}

func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrDraftNotFound):
		writeAppError(w, http.StatusNotFound, "draft_not_found", "no draft has been saved yet")
	case errors.Is(err, store.ErrPackageNotFound):
		writeAppError(w, http.StatusNotFound, "package_not_found", "package does not exist")
	case errors.Is(err, store.ErrGenerationNotFound):
		writeAppError(w, http.StatusNotFound, "generation_not_found", "generation does not exist")
	case errors.Is(err, store.ErrNoCurrent):
		writeAppError(w, http.StatusServiceUnavailable, "no_current",
			"no configuration has been published yet")
	case errors.Is(err, store.ErrClientIncompatible):
		writeAppError(w, http.StatusUnprocessableEntity, "client_incompatible",
			"client version does not satisfy the observed generation's minimum version")
	case errors.Is(err, store.ErrClientNotInTrial):
		writeAppError(w, http.StatusUnprocessableEntity, "client_not_in_trial",
			"bucket and rollout rules show that this client did not receive the trial package")
	case errors.Is(err, store.ErrConflict):
		writeAppError(w, http.StatusConflict, "generation_conflict",
			"expected_gen does not match the current generation; refetch and retry")
	default:
		log.Printf("request %s %s failed: %v", r.Method, r.URL.Path, err)
		writeAppError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}
