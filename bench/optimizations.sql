-- Phase 6 optimization 1 (measured, see docs/17-phase-6-report.md): Q4b went 26389 ms -> 4.1 ms.
-- Cause: tenant_id and type are correlated; the planner misestimated and chose events_retention_idx.
CREATE STATISTICS IF NOT EXISTS events_tenant_type_stats (dependencies, ndistinct, mcv) ON tenant_id, type FROM events;
ANALYZE events;
