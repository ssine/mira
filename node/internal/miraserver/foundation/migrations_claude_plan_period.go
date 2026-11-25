package foundation

// Optional administrator-entered end date of the current Claude plan period.
// Expand-only: older Servers ignore this column.
const claudePlanPeriodSQL = `ALTER TABLE mira_claude_accounts ADD COLUMN plan_period_ends_on DATE;`
