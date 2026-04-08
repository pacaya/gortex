package graph

type NodeKind string

const (
	KindFile      NodeKind = "file"
	KindPackage   NodeKind = "package"
	KindFunction  NodeKind = "function"
	KindMethod    NodeKind = "method"
	KindType      NodeKind = "type"
	KindInterface NodeKind = "interface"
	KindVariable  NodeKind = "variable"
	KindImport    NodeKind = "import"
)

var validNodeKinds = map[NodeKind]bool{
	KindFile: true, KindPackage: true, KindFunction: true,
	KindMethod: true, KindType: true, KindInterface: true,
	KindVariable: true, KindImport: true,
}

type Node struct {
	ID         string         `json:"id"`
	Kind       NodeKind       `json:"kind"`
	Name       string         `json:"name"`
	QualName   string         `json:"qual_name,omitempty"`
	FilePath   string         `json:"file_path"`
	StartLine  int            `json:"start_line"`
	EndLine    int            `json:"end_line"`
	Language   string         `json:"language"`
	Meta       map[string]any `json:"meta,omitempty"`
	RepoPrefix string         `json:"repo_prefix,omitempty"`
}

// Brief returns a compact representation with only the fields needed for listing.
func (n *Node) Brief() map[string]any {
	b := map[string]any{
		"id":         n.ID,
		"name":       n.Name,
		"kind":       n.Kind,
		"file_path":  n.FilePath,
		"start_line": n.StartLine,
	}
	if n.RepoPrefix != "" {
		b["repo_prefix"] = n.RepoPrefix
	}
	return b
}

func ValidNodeKind(k NodeKind) bool {
	return validNodeKinds[k]
}
