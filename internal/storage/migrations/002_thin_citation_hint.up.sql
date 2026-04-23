-- ---------------------------------------------------------------------------
-- 002_thin_citation_hint.up.sql — seed the `inscribe-without-transfer-reasoning`
-- hint rule (QUEST-167).
--
-- Enforces the LORE-312 reasoning-surface convention: when a lore entry
-- cites an ancestor via `informs`, the summary must carry the transfer
-- articulation (why the ancestor applies HERE — delta, inversion,
-- adoption, triviality). The hint fires when the informs list is
-- non-empty AND the summary lacks a transfer marker AND lacks the
-- trivial-transfer escape phrasing.
--
-- Severity: fyi — advisory nudge, not a blocker. Short reports and
-- trivial-transfer entries ("same shape, no delta") are legitimate.
--
-- Detector lives in internal/hints/detectors.go
-- (triggerInscribeWithoutTransferReasoning); rule definition in
-- internal/hints/rules.go.
-- ---------------------------------------------------------------------------

INSERT OR IGNORE INTO hints (rule_id, trigger_tool, severity, template, cooldown_calls, per_era_severity) VALUES
  ('inscribe-without-transfer-reasoning', 'lore_inscribe', 'fyi', 'cites an ancestor via informs but the summary does not articulate the transfer — name why the prior applies HERE (delta, inversion, adoption, or triviality) per the LORE-312 convention',  5, NULL);
