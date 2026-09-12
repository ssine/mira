package node

// This is a Mira launcher default, shared by managed App Server and mira codex.
// Codex counts spawned threads here; multi-agent v2 adds the root internally.
// Use the supported legacy alias so a later explicit override using either the
// legacy or canonical key can take precedence in Codex's CLI configuration layer.
const codexDefaultSubagentOverride = "agents.max_threads=1000"
