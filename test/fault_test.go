package integration

import (
	"net/http"
	"sync"
	"testing"
)

func faultClients(t *testing.T, percent int) (inTrial []string, outsideTrial string) {
	t.Helper()
	ids := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		ids = append(ids, "fault-client-"+itoaTest(i))
	}
	in, out := trialClients(ids, percent)
	if len(in) < 3 || len(out) == 0 {
		t.Fatalf("need at least 3 trial clients and one outside client, got %d/%d", len(in), len(out))
	}
	return in[:3], out[0]
}

func reportFault(f *fixture, base string, clientID, version string, gen int64) (int, map[string]any) {
	return f.do(http.MethodPost, base, "/v1/fault-reports", map[string]any{
		"client_id":      clientID,
		"client_version": version,
		"gen":            gen,
	})
}

func faultReportIDs(body map[string]any) map[string]bool {
	rawReports := body["fault_reports"].([]any)
	out := make(map[string]bool, len(rawReports))
	for _, raw := range rawReports {
		report := raw.(map[string]any)
		out[report["client_id"].(string)] = true
	}
	return out
}

// 11. 故障登记：服务端按所见代次复核分桶和版本；重复上报幂等；第三个不同
// 有效客户端在同一发布轨迹上追加 0% fault_disable 新代次，原代次保持不变。
func TestFaultReportsVerifyAndDisableTrial(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(base, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	f.mustPublishOK(base, pkg2, 30, "1.0.0", 1)

	trialClients, outsideTrial := faultClients(t, 30)
	c1, c2, c3 := trialClients[0], trialClients[1], trialClients[2]

	// 旧代次上报在复核试用资格之前即被判为过期，不能登记或触发处置。
	status, body := reportFault(f, base, c1, "9.0.0", 1)
	f.mustStatus(http.StatusConflict, status, body)
	if body["error"] != "generation_conflict" {
		t.Fatalf("old generation report must conflict: %v", body)
	}

	// 版本不满足所见代次最低版本：即使桶在灰度内也无效。
	status, body = reportFault(f, base, c1, "0.9.0", 2)
	f.mustStatus(http.StatusUnprocessableEntity, status, body)
	if body["error"] != "client_incompatible" {
		t.Fatalf("want client_incompatible, got %v", body["error"])
	}

	// 桶未命中试用：版本再新也不能证明它取得过该试用包。
	status, body = reportFault(f, base, outsideTrial, "9.0.0", 2)
	f.mustStatus(http.StatusUnprocessableEntity, status, body)
	if body["error"] != "client_not_in_trial" {
		t.Fatalf("want client_not_in_trial, got %v", body["error"])
	}

	// 前两个不同有效客户端只登记，不推进代次。
	status, body = reportFault(f, base, c1, "1.0.0", 2)
	f.mustStatus(http.StatusCreated, status, body)
	if body["created"] != true || asInt64(body["count"]) != 1 || body["triggered"] != false {
		t.Fatalf("first report response wrong: %v", body)
	}
	status, body = reportFault(f, base, c2, "9.0.0", 2)
	f.mustStatus(http.StatusCreated, status, body)
	if asInt64(body["count"]) != 2 || body["triggered"] != false {
		t.Fatalf("second report must not trigger: %v", body)
	}

	// 同一客户端对同一代次重复上报只计一次。
	status, body = reportFault(f, base, c1, "1.0.0", 2)
	f.mustStatus(http.StatusOK, status, body)
	if body["duplicate"] != true || body["created"] != false || asInt64(body["count"]) != 2 {
		t.Fatalf("duplicate report response wrong: %v", body)
	}

	// 第三个不同有效客户端触发：响应展示完整依据和新代次。
	status, body = reportFault(f, base, c3, "9.0.0", 2)
	f.mustStatus(http.StatusCreated, status, body)
	if body["triggered"] != true || asInt64(body["count"]) != 3 {
		t.Fatalf("third report should trigger disable: %v", body)
	}
	disabled := body["generation"].(map[string]any)
	if asInt64(disabled["gen"]) != 3 || disabled["kind"] != "fault_disable" ||
		asInt64(disabled["package"]) != pkg2 || asInt64(disabled["trial_percent"]) != 0 ||
		asInt64(disabled["fault_from"]) != 2 {
		t.Fatalf("fault generation metadata wrong: %v", disabled)
	}
	if got := snapshotOf(disabled["snapshot"]); !sameSnapshot(got, snap("v2")) {
		t.Fatalf("fault generation must keep the same complete package: %+v", got)
	}
	ids := faultReportIDs(disabled)
	for _, id := range []string{c1, c2, c3} {
		if !ids[id] {
			t.Fatalf("fault generation evidence missing %s: %v", id, ids)
		}
	}
	if ids[outsideTrial] {
		t.Fatal("outside-trial client must not be included in evidence")
	}

	// 原发布代次不可原地改写；发布轨迹完整保留。
	status, gen2 := f.do(http.MethodGet, base, "/v1/generations/2", nil)
	f.mustStatus(http.StatusOK, status, gen2)
	if asInt64(gen2["trial_percent"]) != 30 || gen2["kind"] != "publish" {
		t.Fatalf("observed generation was mutated: %v", gen2)
	}
	status, list := f.do(http.MethodGet, base, "/v1/generations", nil)
	f.mustStatus(http.StatusOK, status, list)
	gens := list["generations"].([]any)
	if len(gens) != 3 {
		t.Fatalf("want 3 immutable generations, got %d", len(gens))
	}

	// 故障上报查询入口保留完整登记轨迹。
	status, reports := f.do(http.MethodGet, base, "/v1/generations/2/fault-reports", nil)
	f.mustStatus(http.StatusOK, status, reports)
	if asInt64(reports["count"]) != 3 {
		t.Fatalf("want 3 fault reports, got %v", reports["count"])
	}

	// 新代次把同包试用降为零；解析沿既有兼容回退规则选择完整旧包。
	status, decision := f.resolve(base, c1, "9.0.0")
	f.mustStatus(http.StatusOK, status, decision)
	if asInt64(decision["gen"]) != 1 || asInt64(decision["package"]) != pkg1 ||
		decision["fallback"] != true || asInt64(decision["current_gen"]) != 3 {
		t.Fatalf("trial client must fall back after fault disable: %v", decision)
	}
	if got := snapshotOf(decision["snapshot"]); !sameSnapshot(got, snap("v1")) {
		t.Fatalf("fallback snapshot after disable wrong: %+v", got)
	}

	// 停用后再上报旧代次是过期操作；故障记录本身也不可变。
	status, body = reportFault(f, base, c2, "9.0.0", 2)
	f.mustStatus(http.StatusConflict, status, body)
	if _, err := f.db.Exec(`UPDATE fault_reports SET bucket = 0 WHERE id = 1`); err == nil {
		t.Fatal("UPDATE fault_reports must be rejected by immutability trigger")
	}
}

// 12. 第三个故障上报与人工调比例/回滚跨实例交错时，数据库裁决唯一先后顺序；
// 落败方收到可重试冲突，不产生重复代次。
func TestFaultReportConcurrentWithManualGeneration(t *testing.T) {
	for _, manual := range []string{"adjust", "rollback"} {
		t.Run(manual, func(t *testing.T) {
			f := newFixture(t)
			a := f.baseURL()
			b := f.newServer()

			pkg1 := f.freeze(snap("v1"))
			f.mustPublishOK(a, pkg1, 100, "1.0.0", 0)
			pkg2 := f.freeze(snap("v2"))
			f.mustPublishOK(a, pkg2, 30, "1.0.0", 1)

			trialClients, _ := faultClients(t, 30)
			c1, c2, c3 := trialClients[0], trialClients[1], trialClients[2]
			for _, id := range []string{c1, c2} {
				status, body := reportFault(f, a, id, "9.0.0", 2)
				f.mustStatus(http.StatusCreated, status, body)
			}

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var faultStatus, manualStatus int
			go func() {
				defer wg.Done()
				<-start
				faultStatus, _ = reportFault(f, a, c3, "9.0.0", 2)
			}()
			go func() {
				defer wg.Done()
				<-start
				body := map[string]any{"expected_gen": 2}
				if manual == "adjust" {
					body["trial_percent"] = 100
					manualStatus, _ = f.do(http.MethodPost, b, "/v1/adjust", body)
				} else {
					body["gen"] = 1
					manualStatus, _ = f.do(http.MethodPost, b, "/v1/rollback", body)
				}
			}()
			close(start)
			wg.Wait()

			if (faultStatus == http.StatusConflict) == (manualStatus == http.StatusConflict) {
				t.Fatalf("exactly one operation must conflict: fault=%d manual=%d", faultStatus, manualStatus)
			}
			if faultStatus != http.StatusCreated && manualStatus != http.StatusCreated {
				t.Fatalf("one operation must be created: fault=%d manual=%d", faultStatus, manualStatus)
			}

			status, current := f.do(http.MethodGet, a, "/v1/current", nil)
			f.mustStatus(http.StatusOK, status, current)
			if asInt64(current["gen"]) != 3 {
				t.Fatalf("race must create exactly one gen3, current=%v", current)
			}

			if faultStatus == http.StatusCreated {
				if current["kind"] != "fault_disable" || asInt64(current["trial_percent"]) != 0 {
					t.Fatalf("fault winner current wrong: %v", current)
				}
				// 人工操作落败后，旧 expected_gen 仍冲突，读取新代次后可重试。
				stale := map[string]any{"expected_gen": 2}
				path := "/v1/adjust"
				if manual == "adjust" {
					stale["trial_percent"] = 100
				} else {
					path = "/v1/rollback"
					stale["gen"] = 1
				}
				status, body := f.do(http.MethodPost, a, path, stale)
				f.mustStatus(http.StatusConflict, status, body)

				retry := map[string]any{"expected_gen": 3}
				if manual == "adjust" {
					retry["trial_percent"] = 100
				} else {
					path = "/v1/rollback"
					retry["gen"] = 1
				}
				status, body = f.do(http.MethodPost, a, path, retry)
				f.mustStatus(http.StatusCreated, status, body)
				if asInt64(body["gen"]) != 4 {
					t.Fatalf("manual retry must create gen4, got %v", body["gen"])
				}
			} else {
				if current["kind"] != manual || asInt64(current["trial_percent"]) != 100 {
					t.Fatalf("manual winner current wrong: %v", current)
				}
				// 落败故障事务已回滚：gen2 仍只有两条见证。
				status, reports := f.do(http.MethodGet, a, "/v1/generations/2/fault-reports", nil)
				f.mustStatus(http.StatusOK, status, reports)
				if asInt64(reports["count"]) != 2 {
					t.Fatalf("lost fault transaction must roll back its report: %v", reports)
				}
				// 旧所见代次不可重试；读取赢家代次后，第三批有效上报可触发新代次。
				status, body := reportFault(f, a, c3, "9.0.0", 2)
				f.mustStatus(http.StatusConflict, status, body)
				for i, id := range []string{c1, c2, c3} {
					status, body = reportFault(f, a, id, "9.0.0", 3)
					f.mustStatus(http.StatusCreated, status, body)
					if i == 2 {
						if body["triggered"] != true {
							t.Fatalf("third report against winner generation must trigger: %v", body)
						}
						disabled := body["generation"].(map[string]any)
						if asInt64(disabled["gen"]) != 4 || asInt64(disabled["fault_from"]) != 3 ||
							asInt64(disabled["package"]) != asInt64(current["package"]) {
							t.Fatalf("retried fault generation wrong: %v", disabled)
						}
					}
				}
			}

			status, list := f.do(http.MethodGet, a, "/v1/generations", nil)
			f.mustStatus(http.StatusOK, status, list)
			if len(list["generations"].([]any)) != 4 {
				t.Fatalf("race and retry must not duplicate generations: %v", list)
			}
		})
	}
}

// 13. 第三个上报触发新代次时若事务失败，故障见证也必须一起回滚；不存在半份记录。
func TestFaultTriggerTransactionFailureIsAtomic(t *testing.T) {
	f := newFixture(t)
	base := f.baseURL()

	pkg1 := f.freeze(snap("v1"))
	f.mustPublishOK(base, pkg1, 100, "1.0.0", 0)
	pkg2 := f.freeze(snap("v2"))
	f.mustPublishOK(base, pkg2, 30, "1.0.0", 1)
	trialClients, _ := faultClients(t, 30)
	c1, c2, c3 := trialClients[0], trialClients[1], trialClients[2]
	for _, id := range []string{c1, c2} {
		status, body := reportFault(f, base, id, "9.0.0", 2)
		f.mustStatus(http.StatusCreated, status, body)
	}

	// 测试专用触发器让“写 fault_disable 代次”这一步失败；BEFORE INSERT 与
	// 故障见证处于同一业务事务，必须整体回滚。
	mustExec := func(stmt string) {
		t.Helper()
		if _, err := f.db.Exec(stmt); err != nil {
			t.Fatalf("inject failure: %v\n%s", err, stmt)
		}
	}
	mustExec(`DROP TRIGGER IF EXISTS trg_inject_fault_failure ON generations`)
	mustExec(`CREATE OR REPLACE FUNCTION fail_fault_disable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'injected fault_disable failure' USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql`)
	mustExec(`CREATE TRIGGER trg_inject_fault_failure
        BEFORE INSERT ON generations
        FOR EACH ROW
        WHEN (NEW.kind = 'fault_disable')
        EXECUTE FUNCTION fail_fault_disable()`)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DROP TRIGGER IF EXISTS trg_inject_fault_failure ON generations`)
		_, _ = f.db.Exec(`DROP FUNCTION IF EXISTS fail_fault_disable()`)
	})

	status, body := reportFault(f, base, c3, "9.0.0", 2)
	f.mustStatus(http.StatusInternalServerError, status, body)

	// 没有当前指针推进、没有 fault_disable 代次、也没有第三条故障见证。
	status, current := f.do(http.MethodGet, base, "/v1/current", nil)
	f.mustStatus(http.StatusOK, status, current)
	if asInt64(current["gen"]) != 2 {
		t.Fatalf("current must remain gen2 after failed transaction: %v", current)
	}
	status, reports := f.do(http.MethodGet, base, "/v1/generations/2/fault-reports", nil)
	f.mustStatus(http.StatusOK, status, reports)
	if asInt64(reports["count"]) != 2 {
		t.Fatalf("third fault report must roll back, count=%v", reports["count"])
	}
	var faultGenerations int
	if err := f.db.QueryRow(`SELECT count(*) FROM generations WHERE kind = 'fault_disable'`).
		Scan(&faultGenerations); err != nil || faultGenerations != 0 {
		t.Fatalf("want no fault generation, count=%d err=%v", faultGenerations, err)
	}

	// 移除注入失败后，同一第三个见证重新提交成功；此前失败未留下任何半份状态。
	mustExec(`DROP TRIGGER trg_inject_fault_failure ON generations`)
	status, body = reportFault(f, base, c3, "9.0.0", 2)
	f.mustStatus(http.StatusCreated, status, body)
	if body["triggered"] != true {
		t.Fatalf("retry after transaction failure should trigger: %v", body)
	}
	disabled := body["generation"].(map[string]any)
	if asInt64(disabled["trial_percent"]) != 0 || asInt64(disabled["fault_from"]) != 2 {
		t.Fatalf("retried fault generation metadata wrong: %v", disabled)
	}
	status, reports = f.do(http.MethodGet, base, "/v1/generations/2/fault-reports", nil)
	f.mustStatus(http.StatusOK, status, reports)
	if asInt64(reports["count"]) != 3 {
		t.Fatalf("retry should leave exactly 3 reports: %v", reports["count"])
	}
}
