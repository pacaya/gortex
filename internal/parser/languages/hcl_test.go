package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestHCLExtractor_Blocks(t *testing.T) {
	src := []byte(`resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t2.micro"
}

variable "region" {
  default = "us-east-1"
}

output "instance_id" {
  value = aws_instance.web.id
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("main.tf", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 3, "should extract resource, variable, and output blocks")

	// Check that block names include labels.
	names := make(map[string]bool)
	for _, n := range types {
		names[n.Name] = true
	}
	assert.True(t, names["resource.aws_instance.web"], "should have resource.aws_instance.web")
	assert.True(t, names["variable.region"], "should have variable.region")
}

func TestHCLExtractor_ModuleAndData(t *testing.T) {
	src := []byte(`module "vpc" {
  source = "./modules/vpc"
}

data "aws_ami" "ubuntu" {
  most_recent = true
}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("infra.tf", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 2, "should extract module and data blocks")
}

func TestHCLExtractor_FileNode(t *testing.T) {
	src := []byte(`variable "name" {}
`)
	e := NewHCLExtractor()
	result, err := e.Extract("vars.tf", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	assert.Equal(t, 1, len(files))
	assert.Equal(t, "vars.tf", files[0].Name)
}
