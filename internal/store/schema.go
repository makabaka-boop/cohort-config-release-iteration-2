package store

// schema 是全部 DDL。发布时一次性执行（IF NOT EXISTS 保证可重入）。
//
// 模型：
//
//	drafts     —— 单例草稿，routes/limits 可随意覆盖，尚未成包。
//	packages   —— 冻结后的不可变完整快照，包号 pkg 由序列单调分配。
//	generations—— 每一次 publish/adjust/rollback/fault_disable 产生的不可变发布代次记录。
//	rollout_state —— 单行当前指针 + 预期代次（乐观锁）。
//	fault_reports —— 按代次登记、且服务端复核确实命中试用的客户端故障。
//	fault_actions  —— 第三个不同客户端触发自动停用的依据与新代次。
const schema = `
CREATE TABLE IF NOT EXISTS packages (
    pkg          BIGSERIAL PRIMARY KEY,
    content      JSONB     NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS generations (
    gen                BIGSERIAL PRIMARY KEY,
    pkg                BIGINT NOT NULL REFERENCES packages(pkg),
    kind               TEXT NOT NULL,
    trial_percent      INT NOT NULL CHECK (trial_percent BETWEEN 0 AND 100),
    min_major          BIGINT NOT NULL,
    min_minor          BIGINT NOT NULL,
    min_patch          BIGINT NOT NULL,
    min_version_text   TEXT NOT NULL,
    rollback_from      BIGINT REFERENCES generations(gen),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_generations_pkg ON generations(pkg);

-- 旧版本库中的 kind 约束可能是匿名约束；先移除本列上的旧 CHECK，再统一建成具名约束。
DO $$
DECLARE
    c name;
BEGIN
    FOR c IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'generations'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) ILIKE '%kind%'
    LOOP
        EXECUTE 'ALTER TABLE generations DROP CONSTRAINT ' || quote_ident(c);
    END LOOP;
END $$;
ALTER TABLE generations
    ADD CONSTRAINT generations_kind_check
    CHECK (kind IN ('publish','adjust','rollback','fault_disable'));

CREATE TABLE IF NOT EXISTS drafts (
    id        INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    content   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rollout_state (
    id          INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    current_gen BIGINT NOT NULL REFERENCES generations(gen)
);

CREATE TABLE IF NOT EXISTS fault_reports (
    id                   BIGSERIAL PRIMARY KEY,
    gen                  BIGINT NOT NULL REFERENCES generations(gen),
    client_id            TEXT NOT NULL CHECK (length(btrim(client_id)) > 0 AND length(client_id) <= 256),
    client_major         BIGINT NOT NULL,
    client_minor         BIGINT NOT NULL,
    client_patch         BIGINT NOT NULL,
    client_version_text  TEXT NOT NULL,
    bucket               INT NOT NULL CHECK (bucket BETWEEN 0 AND 99),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (gen, client_id)
);
CREATE INDEX IF NOT EXISTS idx_fault_reports_gen ON fault_reports(gen);

CREATE TABLE IF NOT EXISTS fault_actions (
    new_gen          BIGINT PRIMARY KEY REFERENCES generations(gen),
    trigger_gen      BIGINT NOT NULL UNIQUE REFERENCES generations(gen),
    third_report_id  BIGINT NOT NULL REFERENCES fault_reports(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS fault_action_reports (
    new_gen    BIGINT NOT NULL REFERENCES fault_actions(new_gen),
    report_id  BIGINT NOT NULL UNIQUE REFERENCES fault_reports(id),
    PRIMARY KEY (new_gen, report_id)
);

-- 不可变性：包、发布代次与故障证据一经写入，不允许 UPDATE / DELETE。
-- 自动停用只会追加新代次，绝不原地修改已发布代次。
-- （TRUNCATE 仅测试清理时使用，不受行级触发器约束。）
CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_packages_immutable ON packages;
CREATE TRIGGER trg_packages_immutable
    BEFORE UPDATE OR DELETE ON packages
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

DROP TRIGGER IF EXISTS trg_generations_immutable ON generations;
CREATE TRIGGER trg_generations_immutable
    BEFORE UPDATE OR DELETE ON generations
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

DROP TRIGGER IF EXISTS trg_fault_reports_immutable ON fault_reports;
CREATE TRIGGER trg_fault_reports_immutable
    BEFORE UPDATE OR DELETE ON fault_reports
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

DROP TRIGGER IF EXISTS trg_fault_actions_immutable ON fault_actions;
CREATE TRIGGER trg_fault_actions_immutable
    BEFORE UPDATE OR DELETE ON fault_actions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

DROP TRIGGER IF EXISTS trg_fault_action_reports_immutable ON fault_action_reports;
CREATE TRIGGER trg_fault_action_reports_immutable
    BEFORE UPDATE OR DELETE ON fault_action_reports
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
`
