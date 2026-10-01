package config

import "testing"

func validSnapshot() Snapshot {
	return Snapshot{
		Limits: []Limit{
			{ID: "free", QPS: 10, Burst: 20},
			{ID: "paid", QPS: 100, Burst: 200},
		},
		Routes: []Route{
			{Path: "/a", Backend: "svc-a", LimitID: "free"},
			{Path: "/b", Backend: "svc-b", LimitID: "paid"},
		},
	}
}

func TestValidateAcceptsSnapshotWithClosedReferences(t *testing.T) {
	s := validSnapshot()
	if err := s.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

// 跨组引用断裂必须被拒绝：路由引用不存在的限额。
func TestValidateRejectsDanglingCrossGroupReference(t *testing.T) {
	s := validSnapshot()
	s.Routes[0].LimitID = "missing"
	err := s.Validate()
	ve, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	found := false
	for _, f := range ve.Fields {
		if f.Field == "routes[0].limit_id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected error on routes[0].limit_id, got %+v", ve.Fields)
	}
}

func TestValidateRejectsEmptyAndDuplicates(t *testing.T) {
	s := Snapshot{
		Limits: []Limit{{ID: "x", QPS: -1}, {ID: "x"}},
		Routes: []Route{{Path: "/a", Backend: "svc-a", LimitID: "x"}, {Path: "/a", Backend: "svc-b", LimitID: "x"}},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("expected validation errors")
	}
}
