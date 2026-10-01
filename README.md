# Config Release —— routes + limits 原子发布服务

内容站点的 `routes`（路由）与 `limits`（限额）是**同一配置包里不可分割的两组内容**。
系统保证：

- 草稿先通过**跨组引用校验**（每条 route 的 `limit_id` 必须在 limits 中存在），
  才能冻结成**不可变包（package）**；包一经写入不可修改、不可删除。
- 发布、调比例、回滚以及试用故障自动停用都创建**新的发布代次（generation）**，
  每次写操作必须携带 `expected_gen`（故障上报携带所见 `gen`）；两个 API 实例
  同时提交时只有一个能成功，另一方收到 `409 generation_conflict`。
- 发布时声明 **0～100% 试用比例**与**最低客户端版本**。
  客户端按 `FNV-1a/64(client_id) mod 100` 稳定分桶：
  多实例一致、重启后不变，调整比例只移动阈值、不重排客户端。
- 不在试用集合内、或客户端版本不满足最低版本要求时，**回退到最近的兼容包**
  （当前代次之前、包号不同于当前包、最低版本要求不高于客户端版本的最新一代）。
- 所有响应同时给出**实际包号 `package` 与发布代次 `gen`**；
  解析、发布记录、回滚三者永远指向同一个完整快照（routes + limits 成对），
  不存在“新路由配旧限额”。

## 组件

| 路径 | 说明 |
| --- | --- |
| `cmd/server` | HTTP 服务入口（含 `-healthcheck` 自探针） |
| `internal/config` | 快照领域模型与跨组引用校验 |
| `internal/hash` | 客户端稳定分桶（FNV-1a，桶 0..99） |
| `internal/semver` | 最低客户端版本比较 |
| `internal/store` | PostgreSQL 存储：不可变包、代次、行锁 + 乐观并发 |
| `internal/resolve` | 灰度命中与兼容回退解析 |
| `internal/api` | HTTP API |
| `test` | 端到端集成测试（verify 服务运行） |

## 快速开始（固定验收）

```bash
# 1) 校验 compose 文件
docker compose config --quiet

# 2) 构建镜像（db / api / verify）
docker compose build

# 3) 运行一次性验收测试（真实 PostgreSQL + 全部测试）
docker compose run --rm verify
```

启动服务：

```bash
docker compose up -d db api
curl -s http://localhost:8080/healthz
```

## API

所有请求/响应均为 JSON。写操作的 `expected_gen`：首次发布传 `0`，
之后传你刚读到的当前代次；代次不匹配返回 `409`。

### 草稿（可反复改，不影响线上）

```bash
curl -s -XPUT localhost:8080/v1/draft -d '{
  "snapshot": {
    "routes": [{"path":"/a","backend":"svc-a","limit_id":"free"}],
    "limits": [{"id":"free","qps":10,"burst":20}]
  }}'

curl -s -XPOST localhost:8080/v1/draft/validate   # 空 body = 校验当前草稿
curl -s localhost:8080/v1/draft
```

引用断裂（route 指向不存在的 limit）返回 `422` 并列出字段错误，
且无法冻结成包。

### 冻结成不可变包

```bash
curl -s -XPOST localhost:8080/v1/packages
# 201 -> {"package":1,"snapshot":{...routes+limits...}}

curl -s localhost:8080/v1/packages/1
```

包表在数据库层有 `BEFORE UPDATE OR DELETE` 触发器，任何修改尝试都会被拒绝。

### 发布 / 调比例 / 回滚

```bash
# 首次发布：包1，10% 试用，要求客户端 >= 1.4.0
curl -s -XPOST localhost:8080/v1/publish -d '{
  "package": 1, "trial_percent": 10,
  "min_client_version": "1.4.0", "expected_gen": 0}'

# 调整比例（或最低版本）-> 新一代，包不变
curl -s -XPOST localhost:8080/v1/adjust -d '{
  "trial_percent": 100, "expected_gen": 1}'

# 回滚到历史代次的完整快照 -> 新一代（默认 100%，可带 trial_percent）
curl -s -XPOST localhost:8080/v1/rollback -d '{
  "gen": 1, "trial_percent": 100, "expected_gen": 2}'
```

### 试用故障上报与自动停用

```bash
# 客户端提交自己的 ID、版本和“亲眼见到”的发布代次。
curl -s -XPOST localhost:8080/v1/fault-reports -d '{
  "client_id": "phone-123",
  "client_version": "1.5.0",
  "gen": 2}'
```

服务端不会直接相信客户端声称自己拿到试用包，而会在事务中用 `gen=2` 的
试用比例、FNV 分桶和最低版本重新复核：

- 旧代次（不是当前代次）：`409 generation_conflict`，不登记；
- 版本不满足该代次最低版本：`422 client_incompatible`，不登记；
- 桶未命中该代次试用比例：`422 client_not_in_trial`，不登记；
- 同一 `(gen, client_id)` 重复上报：`200 {"created":false,"duplicate":true}`，只计一次。

同一代次收到第三个**不同**的有效客户端后，系统在同一个 PostgreSQL 事务内：

1. 写入第三条 `fault_reports` 故障记录；
2. 只追加一条 `kind="fault_disable"` 的新代次，包不变、`trial_percent=0`；
3. 推进当前指针。原故障代次及任何已发布代次都不会被原地改写。

停用后解析仍走既有规则：新代次的试用集合为空，客户端回退到最近的兼容旧包。
人工调比例 / 回滚与第三份故障上报交错时，它们都在 `rollout_state` 同一行锁后
比较当前代次；落败方收到可重试的 `409 generation_conflict`，不会产生重复代次。

```bash
# 查看某代次的完整故障登记轨迹
curl -s localhost:8080/v1/generations/2/fault-reports

# fault_disable 代次的 fault_from 指向触发代次，fault_reports 给出三份依据。
curl -s localhost:8080/v1/generations/3
```

记录与快照：

```bash
curl -s localhost:8080/v1/current                # 当前代次 + 完整快照
curl -s localhost:8080/v1/generations            # 全部发布记录（含快照）
curl -s localhost:8080/v1/generations/2          # 单个代次（含快照）
```

### 客户端解析

```bash
curl -s 'localhost:8080/v1/resolve?client_id=phone-123&client_version=1.5.0'
```

```json
{
  "gen": 1,
  "package": 1,
  "current_gen": 2,
  "trial": false,
  "fallback": true,
  "bucket": 42,
  "min_client_version": "1.0.0",
  "snapshot": { "routes": ["..."], "limits": ["..."] }
}
```

- `trial=true`：命中当前灰度，`gen == current_gen`；
- `fallback=true`：未命中或版本不达标，拿到最近兼容包；
- 从未发布：`503 no_current`；发布过但无任何兼容包：`404 no_compatible_package`。

## 并发控制的实现要点

`CommitGeneration` 与故障自动停用在单个事务里：

1. `SELECT ... FROM rollout_state WHERE id=1 FOR UPDATE` 串行化所有提交；
2. 比对 `current_gen == expected_gen`（故障上报比对所见 `gen`），不一致即 `409`；
3. 向只增不改的 `generations` 追加记录并推进指针；故障停用时，同一事务先完成
   `fault_reports` 见证写入，再追加 `fault_disable` 代次；
4. 首次发布没有行可锁：依赖 `rollout_state` 主键的唯一插入，
   并发首发中只有一个事务能插入成功，其余得到冲突。

## 测试覆盖

`docker compose run --rm verify` 运行：

- 跨组引用校验（validate 与 freeze 均拒绝断裂引用）；
- 包不可变（DB 触发器拒绝 UPDATE）；
- **并发发布**：8 个请求跨 2 个 API 实例同时以 `expected_gen=0` 提交，恰有 1 个成功；
  adjust 与 rollback 并发同样只有一个成功，旧 `expected_gen` 重试仍 409；
- **旧客户端回退**到最近兼容包，并核对 `package`/`gen`/`fallback`/完整快照；
- **比例边界**：`-1`/`101` 被拒；0% 全员回退、100% 全员当前、37% 与哈希契约逐客户端一致；
- **重启后稳定分组**：关闭并新建 API 实例后，80 个客户端决策逐个不变，
  桶号始终等于无状态 FNV-1a 哈希；
- 查询 / 发布记录 / 回滚三个入口读到同一完整快照；
- **试用故障**：覆盖重复上报、版本不兼容、未命中试用、旧代次上报、第三客户端
  触发 `fault_disable`、两实例竞争（第三上报 vs 调比例/回滚），以及注入事务失败后
  故障见证、新代次和当前指针均无半份记录。

## 本地开发（不用 Docker）

需要 Go 1.22+ 与 PostgreSQL 15+：

```bash
go test ./...                                          # 单元测试
TEST_DATABASE_URL='postgres://postgres@/postgres?sslmode=disable&host=/tmp&port=5433' \
  go test -v ./test/...                                # 集成测试
```
