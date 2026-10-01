package integration

import (
	"net/http"
	"sync"
	"testing"

	"configrelease/internal/config"
)

func mustCreated(f *fixture, status int, body map[string]any) {
	f.t.Helper()
	f.mustStatus(http.StatusCreated, status, body)
}

// 1. 跨组引用校验：路由引用不存在的限额，既不能通过 validate，也不能冻结成包。
func TestDraftCrossGroupValidation(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	bad := config.Snapshot{
		Limits: []Limit{{ID: "only", QPS: 1, Burst: 1}},
		Routes: []Route{{Path: "/x", Backend: "be", LimitID: "ghost"}},
	}
	f.saveDraft(bad)

	status, body := f.do(http.MethodPost, base, "/v1/draft/validate", nil)
	f.mustStatus(http.StatusUnprocessableEntity, status, body)

	status, body = f.do(http.MethodPost, base, "/v1/packages", nil)
	f.mustStatus(http.StatusUnprocessableEntity, status, body)

	// 修好引用后立即放行。
	good := config.Snapshot{
		Limits: []Limit{{ID: "only", QPS: 1, Burst: 1}},
		Routes: []Route{{Path: "/x", Backend: "be", LimitID: "only"}},
	}
	f.saveDraft(good)
	status, body = f.do(http.MethodPost, base, "/v1/packages", nil)
	f.mustStatus(http.StatusCreated, status, body)
}

// 2. 包不可变：包号与内容冻结后不能被修改（再次冻结产生新包号）。
func TestPackagesAreImmutableAndAtomic(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	v1 := snap("v1")
	pkg1 := f.freeze(v1)
	pkg2 := f.freeze(snap("v2"))
	if pkg2 != pkg1+1 {
		t.Fatalf("pkg numbers should be monotonic: %d then %d", pkg1, pkg2)
	}

	// pkg1 读回的内容必须仍是 v1 完整快照（路由+限额成对，未被第二版污染）。
	status, body := f.do(http.MethodGet, base, "/v1/packages/1", nil)
	f.mustStatus(http.StatusOK, status, body)
	if got := snapshotOf(body["snapshot"]); !sameSnapshot(got, v1) {
		t.Fatalf("package 1 mutated: %+v", got)
	}

	// 直接对包表 UPDATE 应当被不可变触发器拒绝。
	if _, err := f.db.Exec(`UPDATE packages SET content = '{}'::jsonb WHERE pkg = 1`); err == nil {
		t.Fatal("UPDATE packages must be rejected by immutability trigger")
	}
}

// 2b. 并发冻结：跨两个实例同时冻结草稿，每次都成功并得到互不相同的新包号，
// 且每个包的内容都是一份自洽的完整快照（锁串行化）。
func TestConcurrentFreezeSerializes(t *testing.T) {
	f := newFixture(t)
	a := f.baseURL()
	b := f.newServer()

	f.saveDraft(snap("v1"))

	const n = 6
	var wg sync.WaitGroup
	wg.Add(n)
	statuses := make([]int, n)
	pkgs := make([]int64, n)
	bases := []string{a, b}
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			status, body := f.do(http.MethodPost, bases[i%2], "/v1/packages", nil)
			statuses[i] = status
			if status == http.StatusCreated {
				pkgs[i] = asInt64(body["package"])
				s := snapshotOf(body["snapshot"])
				if len(s.Routes) == 0 || len(s.Limits) == 0 {
					t.Errorf("frozen package incomplete: %+v", s)
				}
			}
		}()
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i, st := range statuses {
		if st != http.StatusCreated {
			t.Fatalf("freeze %d status = %d, want 201", i, st)
		}
		if pkgs[i] <= 0 || seen[pkgs[i]] {
			t.Fatalf("duplicate/invalid pkg number %v among %v", pkgs[i], pkgs)
		}
		seen[pkgs[i]] = true
	}
}

// 3. 并发发布：两个 API 实例携带同一个 expected_gen 同时提交，只能有一个成功。
func TestConcurrentPublishOnlyOneWins(t *testing.T) {
	f := newFixture(t)
	a := f.baseURL()
	b := f.newServer()

	pkg := f.freeze(snap("v1"))

	const n = 8
	var wg sync.WaitGroup
	type result struct {
		status int
		gen    int64
	}
	results := make([]result, n)
	bases := []string{a, b}
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			// 全部按“首次发布”竞争：expected_gen=0。
			status, body := f.publish(bases[i%2], pkg, 100, "1.0.0", 0)
			r := result{status: status}
			if status == http.StatusCreated {
				r.gen = asInt64(body["gen"])
			}
			results[i] = r
		}()
	}
	wg.Wait()

	created, conflict := 0, 0
	var winnerGen int64
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
			winnerGen = r.gen
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", r.status)
		}
	}
	if created != 1 || conflict != n-1 {
		t.Fatalf("want exactly 1 success and %d conflicts, got %d/%d", n-1, created, conflict)
	}

	// 落后方用赢家代次作为 expected_gen 重试即成功，并产生新一代。
	status, body := f.publish(a, pkg, 100, "1.0.0", winnerGen)
	f.mustStatus(http.StatusCreated, status, body)
	if asInt64(body["gen"]) <= winnerGen {
		t.Fatalf("retry must create a newer gen than %d, got %v", winnerGen, body["gen"])
	}

	// 当前代次与重试结果一致。
	status, cur := f.do(http.MethodGet, a, "/v1/current", nil)
	f.mustStatus(http.StatusOK, status, cur)
	if asInt64(cur["gen"]) != asInt64(body["gen"]) {
		t.Fatalf("current gen %v != retry gen %v", cur["gen"], body["gen"])
	}
}

// 4. 旧客户端回退：不满足最低版本时拿到最近兼容包，响应含实际包号与代次。
func TestOldClientFallsBackToLatestCompatible(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(base, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	status, pub2 := f.publish(base, pkg2, 100, "2.0.0", 1)
	mustCreated(f, status, pub2)

	// 旧客户端 1.5.0 不兼容 v2 → 回退到 gen1/pkg1，fallback=true。
	status, d := f.resolve(base, "legacy-phone", "1.5.0")
	f.mustStatus(http.StatusOK, status, d)
	if asInt64(d["gen"]) != 1 || asInt64(d["package"]) != pkg1 {
		t.Fatalf("old client must get gen1/pkg1, got gen=%v pkg=%v", d["gen"], d["package"])
	}
	if d["fallback"] != true {
		t.Fatal("fallback flag must be true for old client")
	}
	if got := snapshotOf(d["snapshot"]); !sameSnapshot(got, snap("v1")) {
		t.Fatalf("fallback snapshot mismatch: %+v", got)
	}
	if d["current_gen"] != float64(2) {
		t.Fatalf("current_gen must still be reported as 2, got %v", d["current_gen"])
	}

	// 新客户端拿当前包。
	status, d = f.resolve(base, "modern-phone", "2.0.0")
	f.mustStatus(http.StatusOK, status, d)
	if asInt64(d["gen"]) != 2 || asInt64(d["package"]) != pkg2 {
		t.Fatalf("new client must get gen2/pkg2, got %v/%v", d["gen"], d["package"])
	}
	if d["fallback"] == true {
		t.Fatal("new client must not be flagged fallback")
	}
}

// 5. 完全没有兼容包时返回 404 no_compatible_package。
func TestNoCompatiblePackage(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg := f.freeze(snap("v2"))
	f.mustPublishOK(base, pkg, 100, "2.0.0", 0)

	status, body := f.resolve(base, "ancient", "1.0.0")
	f.mustStatus(http.StatusNotFound, status, body)
	if body["error"] != "no_compatible_package" {
		t.Fatalf("want no_compatible_package, got %v", body["error"])
	}
}

// 6. 回滚：新客户端随回滚拿到历史完整快照；回滚本身是不可变的新一代。
func TestRollbackCreatesNewGenerationWithSnapshot(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(base, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	f.mustPublishOK(base, pkg2, 100, "1.0.0", 1)

	// 回滚到 gen1 的完整快照。
	status, rb := f.do(http.MethodPost, base, "/v1/rollback", map[string]any{
		"gen": 1, "expected_gen": 2,
	})
	mustCreated(f, status, rb)
	if asInt64(rb["gen"]) != 3 {
		t.Fatalf("rollback must create new gen, got %v", rb["gen"])
	}
	if asInt64(rb["package"]) != pkg1 {
		t.Fatalf("rollback must point at gen1's package %d, got %v", pkg1, rb["package"])
	}
	if rb["kind"] != "rollback" || asInt64(rb["rollback_from"].(float64)) != 1 {
		t.Fatalf("rollback record metadata wrong: kind=%v from=%v", rb["kind"], rb["rollback_from"])
	}
	if got := snapshotOf(rb["snapshot"]); !sameSnapshot(got, snap("v1")) {
		t.Fatalf("rollback snapshot mismatch: %+v", got)
	}

	// 当前代次与新客户端解析结果都应是回滚后的 pkg1。
	status, cur := f.do(http.MethodGet, base, "/v1/current", nil)
	f.mustStatus(http.StatusOK, status, cur)
	if asInt64(cur["package"]) != pkg1 {
		t.Fatalf("current package should be %d, got %v", pkg1, cur["package"])
	}
	status, d := f.resolve(base, "modern", "9.9.9")
	f.mustStatus(http.StatusOK, status, d)
	if asInt64(d["gen"]) != 3 || asInt64(d["package"]) != pkg1 {
		t.Fatalf("after rollback modern client must get gen3/pkg1, got %v/%v", d["gen"], d["package"])
	}

	// 回滚一个不存在的代次 → 404；用过期 expected_gen → 409。
	status, body := f.do(http.MethodPost, base, "/v1/rollback", map[string]any{
		"gen": 99, "expected_gen": 3,
	})
	f.mustStatus(http.StatusNotFound, status, body)
	status, body = f.do(http.MethodPost, base, "/v1/rollback", map[string]any{
		"gen": 1, "expected_gen": 2,
	})
	f.mustStatus(http.StatusConflict, status, body)
}

// 7. 比例边界：0% 全员回退；100% 全员当前；中间比例按稳定桶划分。
func TestTrialPercentBoundaries(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(base, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))

	// 发布比例非法值必须被拒。
	for _, bad := range []int{-1, 101} {
		status, body := f.publish(base, pkg2, bad, "1.0.0", 1)
		f.mustStatus(http.StatusUnprocessableEntity, status, body)
	}

	// 0%：即使最新客户端也拿当前代次之前的兼容包（这里即 v1）。
	f.mustPublishOK(base, pkg2, 0, "1.0.0", 1)
	ids := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		ids = append(ids, "client-"+itoaTest(i))
	}
	for _, id := range ids {
		status, d := f.resolve(base, id, "9.0.0")
		f.mustStatus(http.StatusOK, status, d)
		if asInt64(d["package"]) != pkg1 {
			t.Fatalf("at 0%% nobody may receive pkg2, %s got %v", id, d["package"])
		}
	}

	// 调整到 37%：桶 < 37 的进，其余回退；服务端分组必须与权威算法一致。
	status, adj := f.do(http.MethodPost, base, "/v1/adjust", map[string]any{
		"trial_percent": 37, "expected_gen": 2,
	})
	mustCreated(f, status, adj)
	if asInt64(adj["gen"]) != 3 {
		t.Fatalf("adjust must create gen3, got %v", adj["gen"])
	}

	wantIn, wantOut := trialClients(ids, 37)
	if len(wantIn) == 0 || len(wantOut) == 0 {
		t.Fatalf("test sample degenerate: in=%d out=%d", len(wantIn), len(wantOut))
	}
	for _, id := range wantIn {
		status, d := f.resolve(base, id, "9.0.0")
		f.mustStatus(http.StatusOK, status, d)
		if asInt64(d["package"]) != pkg2 || d["trial"] != true {
			t.Fatalf("%s bucket %d should be in trial pkg2, got pkg=%v trial=%v",
				id, bucket(id), d["package"], d["trial"])
		}
	}
	for _, id := range wantOut {
		status, d := f.resolve(base, id, "9.0.0")
		f.mustStatus(http.StatusOK, status, d)
		if asInt64(d["package"]) != pkg1 || d["fallback"] != true {
			t.Fatalf("%s bucket %d should fall back to pkg1, got pkg=%v",
				id, bucket(id), d["package"])
		}
	}

	// 100%：全员当前包。
	status, body := f.do(http.MethodPost, base, "/v1/adjust", map[string]any{
		"trial_percent": 100, "expected_gen": 3,
	})
	mustCreated(f, status, body)
	for _, id := range ids {
		status, d := f.resolve(base, id, "9.0.0")
		f.mustStatus(http.StatusOK, status, d)
		if asInt64(d["package"]) != pkg2 {
			t.Fatalf("at 100%% %s must get pkg2, got %v", id, d["package"])
		}
	}
}

// 8. 重启后的稳定分组：换一个全新 API 实例（连接池/内存全新），决策逐个一致。
func TestStableGroupingAcrossRestart(t *testing.T) {
	f := newFixture(t)
	first := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(first, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	f.mustPublishOK(first, pkg2, 42, "1.0.0", 1)

	ids := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		ids = append(ids, "stable-"+itoaTest(i))
	}

	collect := func(base string) map[string]decision {
		out := make(map[string]decision, len(ids))
		for _, id := range ids {
			status, d := f.resolve(base, id, "5.0.0")
			f.mustStatus(http.StatusOK, status, d)
			out[id] = decision{
				gen: asInt64(d["gen"]), pkg: asInt64(d["package"]),
				bucket: asInt64(d["bucket"]), trial: d["trial"] == true,
				fallback: d["fallback"] == true,
			}
		}
		return out
	}

	before := collect(first)

	// “重启”：关闭旧实例，起一个全新进程等价物（无任何共享内存）。
	f.servers[0].Close()
	f.servers = f.servers[1:]
	second := f.newServer()
	after := collect(second)

	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("decision for %s changed across restart: %+v vs %+v", id, want, got)
		}
	}

	// 桶号必须与无状态哈希契约一致，确保不依赖任何服务端记忆。
	for id, d := range after {
		if d.bucket != int64(bucket(id)) {
			t.Fatalf("bucket for %s drifted: %d vs %d", id, d.bucket, bucket(id))
		}
	}
}

// 9. 查询、发布记录与回滚指向同一完整快照：各入口读到的 routes+limits 必须一致。
func TestSnapshotConsistencyAcrossReadPaths(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(base, pkg1, 50, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	f.mustPublishOK(base, pkg2, 50, "1.0.0", 1)

	status, current := f.do(http.MethodGet, base, "/v1/current", nil)
	f.mustStatus(http.StatusOK, status, current)
	status, genList := f.do(http.MethodGet, base, "/v1/generations", nil)
	f.mustStatus(http.StatusOK, status, genList)
	status, genOne := f.do(http.MethodGet, base, "/v1/generations/2", nil)
	f.mustStatus(http.StatusOK, status, genOne)
	status, pkgDoc := f.do(http.MethodGet, base, "/v1/packages/2", nil)
	f.mustStatus(http.StatusOK, status, pkgDoc)

	gens := genList["generations"].([]any)
	listed := gens[1].(map[string]any)

	want := snap("v2")
	for name, doc := range map[string]any{
		"current":     current["snapshot"],
		"list[1]":     listed["snapshot"],
		"generation2": genOne["snapshot"],
		"package2":    pkgDoc["snapshot"],
	} {
		if got := snapshotOf(doc); !sameSnapshot(got, want) {
			t.Fatalf("%s snapshot mismatch: %+v", name, got)
		}
	}

	// 灰度内客户端通过 /v1/resolve 也拿到同一个 v2 快照（含限额与路由两组）。
	status, d := f.resolve(base, "snapshot-check", "1.0.0")
	f.mustStatus(http.StatusOK, status, d)
	if asInt64(d["package"]) == pkg2 {
		resolved := snapshotOf(d["snapshot"])
		if !sameSnapshot(resolved, want) {
			t.Fatalf("resolved trial snapshot mismatch: %+v", resolved)
		}
		if len(resolved.Routes) == 0 || len(resolved.Limits) == 0 {
			t.Fatalf("snapshot must contain both groups: %+v", resolved)
		}
	}

	// 回滚记录里的 snapshot 与它指向的历史包完全一致。
	status, rb := f.do(http.MethodPost, base, "/v1/rollback", map[string]any{
		"gen": 2, "expected_gen": 2,
	})
	mustCreated(f, status, rb)
	if got := snapshotOf(rb["snapshot"]); !sameSnapshot(got, want) {
		t.Fatalf("rollback record snapshot mismatch: %+v", got)
	}
}

// 10. 并发的 adjust / rollback 同样受 expected_gen 保护。
func TestConcurrentAdjustAndRollback(t *testing.T) {
	f := newFixture(t)
	a := f.baseURL()
	b := f.newServer()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(a, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	f.mustPublishOK(a, pkg2, 10, "1.0.0", 1)

	var wg sync.WaitGroup
	wg.Add(2)
	var adjustStatus, rollbackStatus int
	go func() {
		defer wg.Done()
		adjustStatus, _ = f.do(http.MethodPost, a, "/v1/adjust", map[string]any{
			"trial_percent": 50, "expected_gen": 2,
		})
	}()
	go func() {
		defer wg.Done()
		rollbackStatus, _ = f.do(http.MethodPost, b, "/v1/rollback", map[string]any{
			"gen": 1, "expected_gen": 2,
		})
	}()
	wg.Wait()

	if (adjustStatus == http.StatusConflict) == (rollbackStatus == http.StatusConflict) {
		t.Fatalf("exactly one should conflict: adjust=%d rollback=%d", adjustStatus, rollbackStatus)
	}
	if adjustStatus != http.StatusCreated && rollbackStatus != http.StatusCreated {
		t.Fatalf("one must succeed: adjust=%d rollback=%d", adjustStatus, rollbackStatus)
	}

	// 负方不能拿旧 expected_gen 再蒙混过关。
	status, body := f.do(http.MethodPost, a, "/v1/adjust", map[string]any{
		"trial_percent": 60, "expected_gen": 2,
	})
	f.mustStatus(http.StatusConflict, status, body)

	// 用赢家代次重试成功，产生更新的一代。
	status, cur := f.do(http.MethodGet, a, "/v1/current", nil)
	f.mustStatus(http.StatusOK, status, cur)
	winnerGen := asInt64(cur["gen"])
	status, body = f.do(http.MethodPost, a, "/v1/adjust", map[string]any{
		"trial_percent": 60, "expected_gen": winnerGen,
	})
	f.mustStatus(http.StatusCreated, status, body)
	if asInt64(body["gen"]) <= winnerGen {
		t.Fatalf("expected a newer gen than %d, got %v", winnerGen, body["gen"])
	}
}

type decision struct {
	gen, pkg, bucket int64
	trial, fallback  bool
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
