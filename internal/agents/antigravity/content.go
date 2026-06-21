// Package antigravity implements the Gortex init integration for
// Google's Antigravity. Today we write a Knowledge Item at
// ~/.gemini/antigravity/knowledge/gortex-workflow/ — the official
// documented mechanism for teaching Antigravity about project-
// specific tooling. The Step 3 audit will add true MCP registration
// alongside the KI once Antigravity's MCP config path is verified.
package antigravity

// Metadata is the KI manifest. Antigravity reads it to show the KI
// in its UI and to locate the artifact files it references.
const Metadata = `{
  "summary": "MANDATORY: Instructions on how to use the local gortex engine CLI to significantly improve codebase intelligence. Antigravity must use run_command with gortex query over standard file read commands.",
  "references": ["artifacts/gortex-instructions.md"]
}
`

// Instructions is the KI body — a Knowledge Item in Antigravity's
// frontmatter-first markdown format. Lists the run_command
// invocations Antigravity should prefer over grep_search / view_file.
const Instructions = `---
type: "Knowledge Item"
description: "Gortex Workflow and Tools for Antigravity"
---

# Gortex Engine Usage

This codebase is indexed by Gortex, an in-memory knowledge graph code intelligence engine.

**MANDATORY:** You MUST prioritize using the Gortex CLI via ` + "`run_command`" + ` instead of using standard text searches (` + "`grep_search`" + `) or full file reads (` + "`view_file`" + `). This drastically improves token efficiency and prevents hallucination.

## Setup
If the daemon is not tracking your target repos, register the current repo so the graph tools activate:
` + "```bash" + `
gortex track .
` + "```" + `

## Standard Workflow Translation

| Instead of... | You MUST use... (via ` + "`run_command`" + `) |
|---|---|
| ` + "`grep_search`" + ` to find a class or function | ` + "`./gortex query symbol <name> --format text`" + ` (AST-aware search) |
| ` + "`grep_search`" + ` to find all references | ` + "`./gortex query usages <id>`" + ` (zero false positives) |
| ` + "`view_file`" + ` to read a whole file to find a method | ` + "`./gortex query symbol <name>`" + ` or ` + "`./gortex query callers <func_id>`" + ` |
| Guessing what breaks during a refactor | ` + "`./gortex query dependents <id>`" + ` (impact analysis) |
| Creating circular dependencies | Evaluate ` + "`./gortex query deps <id>`" + ` first |

## Example Usage

### 1. View Architecture and Communities
` + "```bash" + `
./gortex query stats
` + "```" + `

### 2. Find specific symbol definition
` + "```bash" + `
./gortex query symbol MyController
` + "```" + `

### 3. Trace blast radius
If you are modifying ` + "`core/parser.go::Parse`" + `, check what will break:
` + "```bash" + `
./gortex query dependents core/parser.go::Parse --depth 2
` + "```" + `

This gives you perfectly accurate AST-level analysis, guaranteeing safe edits.
`
