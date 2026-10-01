package store

// schema 是全部 DDL。发布时一次性执行（IF NOT EXISTS 保证可重入）。
//
// 模型：
//
//	drafts     —— 单例草稿，routes/limits 可随意覆盖，尚未成包。
//	packages   —— 冻结后的不可变完整快照，包号 pkg 由序列单调分配。
//	generations—— 每一次 publish/adjust/rollback 产生的不可变发布代次记录。
//	rollout_state —— 单行当前指针 + 预期代次（乐观锁）。
const schema = `
CREATE TABLE IF NOT EXISTS packages (
    pkg          BIGSERIAL PRIMARY KEY,
    content      JSONB     NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS generations (
    gen                BIGSERIAL PRIMARY KEY,
    pkg                BIGINT NOT NULL REFERENCES packages(pkg),
    kind               TEXT NOT NULL CHECK (kind IN ('publish','adjust','rollback')),
    trial_percent      INT NOT NULL CHECK (trial_percent BETWEEN 0 AND 100),
    min_major          BIGINT NOT NULL,
    min_minor          BIGINT NOT NULL,
    min_patch          BIGINT NOT NULL,
    min_version_text   TEXT NOT NULL,
    rollback_from      BIGINT REFERENCES generations(gen),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_generations_pkg ON generations(pkg);

CREATE TABLE IF NOT EXISTS drafts (
    id        INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    content   JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rollout_state (
    id          INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    current_gen BIGINT NOT NULL REFERENCES generations(gen)
);

-- 不可变性：包与发布代次一经写入，不允许 UPDATE / DELETE。
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
`
