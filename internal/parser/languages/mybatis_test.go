package languages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

const sampleMyBatisMapper = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE mapper PUBLIC "-//mybatis.org//DTD Mapper 3.0//EN"
        "http://mybatis.org/dtd/mybatis-3-mapper.dtd">
<mapper namespace="com.app.UserMapper">
    <select id="findUser" resultType="com.app.User">
        SELECT id, name FROM users WHERE id = #{id}
    </select>
    <insert id="insertUser">
        INSERT INTO users (name) VALUES (#{name})
    </insert>
    <update id="updateUser">
        UPDATE users SET name = #{name}
        <where>
            <if test="id != null">id = #{id}</if>
        </where>
    </update>
    <delete id="deleteUser">DELETE FROM users WHERE id = #{id}</delete>
    <sql id="cols">id, name</sql>
    <resultMap id="userMap" type="com.app.User"/>
</mapper>`

func TestMyBatisExtractor_StatementNodes(t *testing.T) {
	res, err := NewMyBatisExtractor().Extract("UserMapper.xml", []byte(sampleMyBatisMapper))
	require.NoError(t, err)

	var file *graph.Node
	stmts := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		switch n.Kind {
		case graph.KindFile:
			file = n
		case graph.KindMethod:
			stmts[n.ID] = n
		}
	}
	require.NotNil(t, file)
	require.Equal(t, "com.app.UserMapper", file.Meta["mybatis_namespace"])

	// One node per <select>/<insert>/<update>/<delete> — <sql> and
	// <resultMap> are excluded.
	require.Len(t, stmts, 4)

	cases := []struct {
		id      string
		kind    string
		sqlPart string
	}{
		{"com.app.UserMapper::findUser", "select", "SELECT id, name FROM users"},
		{"com.app.UserMapper::insertUser", "insert", "INSERT INTO users"},
		{"com.app.UserMapper::updateUser", "update", "UPDATE users SET name"},
		{"com.app.UserMapper::deleteUser", "delete", "DELETE FROM users"},
	}
	for _, c := range cases {
		n := stmts[c.id]
		require.NotNil(t, n, "missing statement node %s", c.id)
		require.Equal(t, c.kind, n.Meta["mybatis_sql_kind"])
		require.Equal(t, "com.app.UserMapper", n.Meta["mybatis_namespace"])
		sql, _ := n.Meta["mybatis_sql"].(string)
		require.Contains(t, sql, c.sqlPart, "statement %s SQL", c.id)
	}

	// The dynamic-SQL <where>/<if> body is flattened into the stored SQL.
	require.Contains(t, stmts["com.app.UserMapper::updateUser"].Meta["mybatis_sql"], "id = #{id}")

	// Each statement emits a placeholder call edge keyed by namespace::id.
	placeholders := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			if via, _ := e.Meta["via"].(string); via == "mybatis.mapper" {
				placeholders[e.To] = true
			}
		}
	}
	require.True(t, placeholders["unresolved::mybatis::com.app.UserMapper::findUser"])
	require.Len(t, placeholders, 4)
}

func TestMyBatisExtractor_NonMapperXMLYieldsOnlyFileNode(t *testing.T) {
	plain := []byte(`<?xml version="1.0"?><config><setting name="x">1</setting></config>`)
	res, err := NewMyBatisExtractor().Extract("config.xml", plain)
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1) // file node only
	require.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	require.Empty(t, res.Edges)
}

func TestIsMyBatisMapper(t *testing.T) {
	require.True(t, IsMyBatisMapper([]byte(`<mapper namespace="com.app.X">`)))
	require.True(t, IsMyBatisMapper([]byte(`<!DOCTYPE mapper PUBLIC "-//mybatis.org//DTD Mapper 3.0//EN" "...">`)))
	require.False(t, IsMyBatisMapper([]byte(`<config><x/></config>`)))
	require.False(t, IsMyBatisMapper([]byte(`<mapper>no namespace here</mapper>`)))
}

func TestMyBatisExtractor_Malformed(t *testing.T) {
	res, err := NewMyBatisExtractor().Extract("bad.xml", []byte(`<mapper namespace="com.app.X"><select id="q">SELECT 1`))
	require.NoError(t, err)        // never a hard failure
	require.NotEmpty(t, res.Nodes) // at least the file node survives
}

func TestMyBatisExtractor_Extensions(t *testing.T) {
	require.Equal(t, []string{".xml"}, NewMyBatisExtractor().Extensions())
}
