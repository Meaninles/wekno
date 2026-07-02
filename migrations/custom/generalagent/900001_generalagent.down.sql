-- Custom migration rollback: general-agent artifacts.
-- Development-only artifacts are disposable; production rollout should archive
-- files before applying this rollback.

DROP TABLE IF EXISTS custom_general_agent_artifacts;
