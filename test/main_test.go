// Package integration 是 verify 一次性服务运行的端到端测试：
// 真实 PostgreSQL + 完整 HTTP 链路 + 多实例并发。
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"configrelease/internal/api"
	"configrelease/internal/config"
	hashpkg "configrelease/internal/hash"
	"configrelease/internal/resolve"
	"configrelease/internal/store"
)

var (
	adminDB *sql.DB
	testDSN string
)

func TestMain(m *testing.M) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "postgres://postgres@/postgres?sslmode=disable&host=/tmp&port=5433"
	}

	admin, err := sql.Open("postgres", base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin db: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "postgres not reachable (%s): %v\n", base, err)
		os.Exit(1)
	}

	// 每次 verify 使用独立库，互不污染。
	dbName := os.Getenv("TEST_DB_NAME")
	if dbName == "" {
		dbName = "configverify"
	}
	_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName)
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		fmt.Fprintf(os.Stderr, "create test db: %v\n", err)
		os.Exit(1)
	}
	_ = admin.Close()

	u, err := url.Parse(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse dsn: %v\n", err)
		os.Exit(1)
	}
	u.Path = "/" + dbName
	testDSN = u.String()

	os.Exit(m.Run())
}

// fixture 管理每个测试独立的 *sql.DB 与多个 API 实例。
type fixture struct {
	t       *testing.T
	db      *sql.DB
	st      *store.Store
	servers []*httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sql.Open("postgres", testDSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(10)
	st := store.New(db)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.ResetForTest(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	f := &fixture{t: t, db: db, st: st}
	t.Cleanup(func() {
		for _, s := range f.servers {
			s.Close()
		}
		_ = db.Close()
	})
	return f
}

// newServer 再起一个独立 API 实例（独立连接池），模拟多副本部署。
func (f *fixture) newServer() string {
	db, err := sql.Open("postgres", testDSN)
	if err != nil {
		f.t.Fatalf("open instance db: %v", err)
	}
	db.SetMaxOpenConns(5)
	st := store.New(db)
	srv := httptest.NewServer(api.NewServer(st, resolve.New(st)))
	f.servers = append(f.servers, srv)
	return srv.URL
}

func (f *fixture) baseURL() string { return f.newServer() }

func (f *fixture) reset() {
	if err := f.st.ResetForTest(context.Background()); err != nil {
		f.t.Fatalf("reset: %v", err)
	}
}

// ---- HTTP 小工具 ----

func (f *fixture) do(method, base, path string, body any) (int, map[string]any) {
	f.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, base+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (f *fixture) mustStatus(want int, got int, body map[string]any) {
	f.t.Helper()
	if got != want {
		f.t.Fatalf("status = %d, want %d; body=%v", got, want, body)
	}
}

func (f *fixture) saveDraft(snap config.Snapshot) {
	f.t.Helper()
	status, body := f.do(http.MethodPut, f.servers[0].URL, "/v1/draft",
		map[string]any{"snapshot": snap})
	f.mustStatus(http.StatusOK, status, body)
}

// freeze 保存草稿并冻结成包，返回新包号。
func (f *fixture) freeze(snap config.Snapshot) int64 {
	f.t.Helper()
	f.saveDraft(snap)
	status, body := f.do(http.MethodPost, f.servers[0].URL, "/v1/packages", nil)
	f.mustStatus(http.StatusCreated, status, body)
	return asInt64(body["package"])
}

func (f *fixture) publish(base string, pkg int64, percent int, minVer string, expectedGen int64) (int, map[string]any) {
	return f.do(http.MethodPost, base, "/v1/publish", map[string]any{
		"package":            pkg,
		"trial_percent":      percent,
		"min_client_version": minVer,
		"expected_gen":       expectedGen,
	})
}

// mustPublish 断言发布成功并返回响应体。
func (f *fixture) mustPublish(base string, pkg int64, percent int, minVer string, expectedGen int64) map[string]any {
	f.t.Helper()
	status, body := f.publish(base, pkg, percent, minVer, expectedGen)
	f.mustStatus(http.StatusCreated, status, body)
	return body
}

// mustPublishOK 仅断言发布成功（忽略响应体）。
func (f *fixture) mustPublishOK(base string, pkg int64, percent int, minVer string, expectedGen int64) {
	f.t.Helper()
	_ = f.mustPublish(base, pkg, percent, minVer, expectedGen)
}

// resolve 对某实例发起解析请求。
func (f *fixture) resolve(base, clientID, ver string) (int, map[string]any) {
	return f.do(http.MethodGet, base,
		"/v1/resolve?client_id="+url.QueryEscape(clientID)+"&client_version="+url.QueryEscape(ver), nil)
}

// reportFault 以客户端实际看到的代次提交故障。
func (f *fixture) reportFault(base, clientID, ver string, observedGen int64) (int, map[string]any) {
	return f.do(http.MethodPost, base, "/v1/fault-reports", map[string]any{
		"client_id":      clientID,
		"client_version": ver,
		"observed_gen":   observedGen,
	})
}

// ---- 构造数据 ----

func snap(version string) config.Snapshot {
	return config.Snapshot{
		Limits: []Limit{{ID: "lim-" + version, QPS: 1, Burst: 1}},
		Routes: []Route{{Path: "/path/" + version, Backend: "be-" + version, LimitID: "lim-" + version}},
	}
}

// use config.Limit/Route via alias for readability
type (
	Limit = config.Limit
	Route = config.Route
)

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func snapshotOf(v any) config.Snapshot {
	raw, _ := json.Marshal(v)
	var s config.Snapshot
	_ = json.Unmarshal(raw, &s)
	return s
}

func sameSnapshot(a, b config.Snapshot) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}

// clientIDsInTrial 用权威算法反推某比例下进入灰度的客户端。
func trialClients(ids []string, percent int) (in, out []string) {
	for _, id := range ids {
		b := bucket(id)
		if int(b) < percent {
			in = append(in, id)
		} else {
			out = append(out, id)
		}
	}
	sort.Strings(in)
	sort.Strings(out)
	return in, out
}

// bucket 直接使用生产代码的哈希契约做断言，
// 保证“服务端分组”和“测试期望”是同一套无状态算法。
func bucket(clientID string) uint64 {
	return hashpkg.Bucket(clientID)
}
