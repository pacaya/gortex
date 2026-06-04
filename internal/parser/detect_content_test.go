package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ambiguityRegistry builds a registry whose extension map exercises
// the ambiguous extensions (.h -> c, .m -> objc) and registers every
// language a content probe can resolve to.
func ambiguityRegistry() *Registry {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "c", exts: []string{".c", ".h"}})
	r.Register(&mockExtractor{lang: "objc", exts: []string{".m", ".mm"}})
	r.Register(&mockExtractor{lang: "cpp", exts: []string{".cpp", ".hpp"}})
	r.Register(&mockExtractor{lang: "matlab", exts: []string{".mlx"}})
	r.Register(&mockExtractor{lang: "mathematica", exts: []string{".wl"}})
	r.Register(&mockExtractor{lang: "perl", exts: []string{".pl"}})
	r.Register(&mockExtractor{lang: "python", exts: []string{".py"}})
	r.Register(&mockExtractor{lang: "bash", exts: []string{".sh"}})
	// .xml defaults to the generic xml extractor; a MyBatis mapper is
	// content-routed to "mybatis" (which claims no extension here, so it
	// never overrides the .xml default).
	r.Register(&mockExtractor{lang: "mybatis"})
	r.Register(&mockExtractor{lang: "spring"})
	r.Register(&mockExtractor{lang: "xml", exts: []string{".xml"}})
	return r
}

func TestSniffShebang(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{"direct bash", "#!/bin/bash\necho hi\n", "bash", true},
		{"direct sh", "#!/bin/sh\n", "bash", true},
		{"env python3", "#!/usr/bin/env python3\n", "python", true},
		{"env python", "#!/usr/bin/env python\n", "python", true},
		{"direct perl", "#!/usr/bin/perl -w\n", "perl", true},
		{"env -S flag", "#!/usr/bin/env -S python3 -u\n", "python", true},
		{"env var assignment", "#!/usr/bin/env FOO=bar ruby\n", "ruby", true},
		{"versioned interpreter", "#!/usr/bin/python3.11\n", "python", true},
		{"node", "#!/usr/bin/env node\n", "javascript", true},
		{"no shebang", "echo hi\n", "", false},
		{"empty", "", "", false},
		{"unknown interpreter", "#!/usr/bin/env cobol-run\n", "", false},
		{"hash but no bang", "# a comment\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lang, ok := sniffShebang([]byte(tc.content))
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, lang)
		})
	}

	// A UTF-8 BOM ahead of the shebang must not defeat detection.
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte("#!/bin/bash\n")...)
	lang, ok := sniffShebang(bom)
	assert.True(t, ok)
	assert.Equal(t, "bash", lang)
}

func TestDetectLanguageContent_Shebang(t *testing.T) {
	r := ambiguityRegistry()

	// A .cgi file has no extension mapping — the shebang resolves it.
	lang, ok := r.DetectLanguageContent("cgi-bin/handler.cgi", []byte("#!/usr/bin/perl\nprint \"hi\";\n"))
	assert.True(t, ok)
	assert.Equal(t, "perl", lang)

	// An extensionless script is resolved purely by its shebang.
	lang, ok = r.DetectLanguageContent("bin/deploy", []byte("#!/usr/bin/env bash\nset -e\n"))
	assert.True(t, ok)
	assert.Equal(t, "bash", lang)

	// An unknown extension with no shebang stays unresolved.
	_, ok = r.DetectLanguageContent("data/notes.cgi", []byte("just some text\n"))
	assert.False(t, ok)

	// A shebang mapping to an unregistered language is ignored.
	bare := NewRegistry()
	bare.Register(&mockExtractor{lang: "go", exts: []string{".go"}})
	_, ok = bare.DetectLanguageContent("x.cgi", []byte("#!/usr/bin/perl\n"))
	assert.False(t, ok)
}

func TestDetectLanguageContent_AmbiguousHeader(t *testing.T) {
	r := ambiguityRegistry()

	// Plain C header — no probe signal, keeps the extension default.
	lang, ok := r.DetectLanguageContent("inc/list.h", []byte("int list_len(struct list *l);\n"))
	assert.True(t, ok)
	assert.Equal(t, "c", lang)

	// C++ header — a namespace / class gives it away.
	lang, ok = r.DetectLanguageContent("inc/widget.h", []byte("namespace ui {\nclass Widget {};\n}\n"))
	assert.True(t, ok)
	assert.Equal(t, "cpp", lang)

	// C++ header detected via the :: scope operator alone.
	lang, ok = r.DetectLanguageContent("inc/util.h", []byte("inline int f() { return std::max(1, 2); }\n"))
	assert.True(t, ok)
	assert.Equal(t, "cpp", lang)

	// Objective-C header.
	lang, ok = r.DetectLanguageContent("inc/View.h", []byte("#import <Foundation/Foundation.h>\n@interface View\n@end\n"))
	assert.True(t, ok)
	assert.Equal(t, "objc", lang)
}

func TestDetectLanguageContent_AmbiguousDotM(t *testing.T) {
	r := ambiguityRegistry()

	// Objective-C implementation.
	lang, ok := r.DetectLanguageContent("View.m", []byte("#import \"View.h\"\n@implementation View\n@end\n"))
	assert.True(t, ok)
	assert.Equal(t, "objc", lang)

	// MATLAB function file.
	lang, ok = r.DetectLanguageContent("solve.m", []byte("function y = solve(x)\n  y = x .^ 2;\nend\n"))
	assert.True(t, ok)
	assert.Equal(t, "matlab", lang)

	// MATLAB class file.
	lang, ok = r.DetectLanguageContent("Point.m", []byte("classdef Point\n  properties\n    x\n  end\nend\n"))
	assert.True(t, ok)
	assert.Equal(t, "matlab", lang)

	// Mathematica package.
	lang, ok = r.DetectLanguageContent("Pkg.m", []byte("BeginPackage[\"Pkg`\"]\nf[x_] := x^2\nEndPackage[]\n"))
	assert.True(t, ok)
	assert.Equal(t, "mathematica", lang)

	// No probe signal — .m keeps its Objective-C default.
	lang, ok = r.DetectLanguageContent("plain.m", []byte("int main() { return 0; }\n"))
	assert.True(t, ok)
	assert.Equal(t, "objc", lang)
}

func TestDetectLanguageContent_MyBatisMapper(t *testing.T) {
	r := ambiguityRegistry()

	mapper := []byte(`<?xml version="1.0"?>
<!DOCTYPE mapper PUBLIC "-//mybatis.org//DTD Mapper 3.0//EN" "...">
<mapper namespace="com.app.UserMapper">
  <select id="findUser" resultType="User">SELECT * FROM users WHERE id = #{id}</select>
</mapper>`)
	lang, ok := r.DetectLanguageContent("UserMapper.xml", mapper)
	assert.True(t, ok)
	assert.Equal(t, "mybatis", lang, "a mapper document routes to the mybatis extractor")

	// A Spring beans XML routes to the spring extractor.
	beans := []byte(`<?xml version="1.0"?>
<beans xmlns="http://www.springframework.org/schema/beans">
  <bean id="svc" class="com.app.Svc"/>
</beans>`)
	lang, ok = r.DetectLanguageContent("applicationContext.xml", beans)
	assert.True(t, ok)
	assert.Equal(t, "spring", lang)

	// A generic XML document keeps the xml default.
	lang, ok = r.DetectLanguageContent("config.xml", []byte(`<?xml version="1.0"?><config><name>x</name></config>`))
	assert.True(t, ok)
	assert.Equal(t, "xml", lang)
}

func TestDetectLanguageContent_NilContentMatchesNameOnly(t *testing.T) {
	r := ambiguityRegistry()

	// With nil content, DetectLanguageContent must agree with the
	// name-only DetectLanguage for every kind of path.
	for _, p := range []string{"a.h", "b.m", "c.py", "d.unknown", "e.cgi"} {
		nameLang, nameOK := r.DetectLanguage(p)
		contentLang, contentOK := r.DetectLanguageContent(p, nil)
		assert.Equal(t, nameOK, contentOK, p)
		assert.Equal(t, nameLang, contentLang, p)
	}

	// A known extension still wins even when content would otherwise
	// probe — a probe never fires for an unambiguous extension.
	lang, ok := r.DetectLanguageContent("script.py", []byte("#!/usr/bin/env bash\n"))
	assert.True(t, ok)
	assert.Equal(t, "python", lang)
}
