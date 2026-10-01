// Package config defines the domain model for routes+limits configuration.
//
// routes 与 limits 是同一配置包（Snapshot）里不可分割的两组内容：
// 任何一次解析、发布记录或回滚拿到的永远是一个完整快照，
// 不可能出现“新路由 + 旧限额”的混搭。
package config

import (
	"errors"
	"fmt"
)

// Limit 是某一个限额组的内容。
type Limit struct {
	ID    string `json:"id"`
	QPS   int64  `json:"qps"`
	Burst int64  `json:"burst"`
}

// Route 是某一条路由，必须通过 LimitID 引用 limits 组内存在的限额。
type Route struct {
	Path    string `json:"path"`
	Backend string `json:"backend"`
	LimitID string `json:"limit_id"`
}

// Snapshot 是冻结后不可变的完整配置包内容。
type Snapshot struct {
	Routes []Route `json:"routes"`
	Limits []Limit `json:"limits"`
}

// FieldError 记录某个字段的校验问题，便于 API 直接返回。
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError 聚合一次校验中发现的全部问题。
type ValidationError struct {
	Fields []FieldError `json:"fields"`
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed: %d field(s) invalid", len(e.Fields))
}

// add 追加一条字段错误。
func (e *ValidationError) add(field, msg string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

// ok 返回校验是否没有任何错误。
func (e *ValidationError) ok() bool { return len(e.Fields) == 0 }

// Validate 校验草稿快照：
//  1. 组内结构合法（ID/路径非空、限额非负、ID 不重复）；
//  2. 跨组引用闭合 —— 每条 route 的 limit_id 必须能在 limits 中找到。
//
// 这正是“草稿校验跨组引用后才能成为不可变包”的那道关口。
func (s *Snapshot) Validate() error {
	ve := &ValidationError{}

	limitByID := make(map[string]int, len(s.Limits))
	for i, l := range s.Limits {
		field := fmt.Sprintf("limits[%d]", i)
		if l.ID == "" {
			ve.add(field+".id", "must not be empty")
		} else if _, dup := limitByID[l.ID]; dup {
			ve.add(field+".id", "duplicate limit id: "+l.ID)
		} else {
			limitByID[l.ID] = i
		}
		if l.QPS < 0 {
			ve.add(field+".qps", "must be >= 0")
		}
		if l.Burst < 0 {
			ve.add(field+".burst", "must be >= 0")
		}
	}

	routePath := make(map[string]int, len(s.Routes))
	for i, r := range s.Routes {
		field := fmt.Sprintf("routes[%d]", i)
		if r.Path == "" {
			ve.add(field+".path", "must not be empty")
		} else if _, dup := routePath[r.Path]; dup {
			ve.add(field+".path", "duplicate route path: "+r.Path)
		} else {
			routePath[r.Path] = i
		}
		if r.Backend == "" {
			ve.add(field+".backend", "must not be empty")
		}
		// 跨组引用：引用不到限额的路由绝不允许冻结成包。
		if r.LimitID == "" {
			ve.add(field+".limit_id", "must reference a limit")
		} else if _, ok := limitByID[r.LimitID]; !ok {
			ve.add(field+".limit_id", "references unknown limit: "+r.LimitID)
		}
	}

	if ve.ok() {
		return nil
	}
	return ve
}

// AsValidationError 把校验错误取出来，供 API 层映射成 422。
func AsValidationError(err error) (*ValidationError, bool) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
