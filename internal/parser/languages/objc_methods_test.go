package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestObjCExtractor_MethodKinds(t *testing.T) {
	const objc = `@implementation Factory
+ (instancetype)shared {
    return nil;
}
- (NSString *)name {
    return @"x";
}
- (nullable NSArray<User *> *)users {
    return nil;
}
@end
`
	res, err := NewObjCExtractor().Extract("Factory.m", []byte(objc))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byName[n.Name] = n
	}

	shared := byName["shared"]
	if shared == nil {
		t.Fatal("method 'shared' was not extracted")
	}
	if shared.Meta["is_static"] != true {
		t.Errorf("+shared should be a class method (is_static): meta=%v", shared.Meta)
	}
	if shared.Meta["return_type"] != "instancetype" {
		t.Errorf("shared return_type = %v, want instancetype", shared.Meta["return_type"])
	}

	name := byName["name"]
	if name == nil {
		t.Fatal("method 'name' was not extracted")
	}
	if name.Meta["is_static"] == true {
		t.Errorf("-name should be an instance method, not static: meta=%v", name.Meta)
	}
	if name.Meta["return_type"] != "NSString" {
		t.Errorf("name return_type = %v, want NSString (pointer stripped)", name.Meta["return_type"])
	}

	users := byName["users"]
	if users == nil {
		t.Fatal("method 'users' was not extracted")
	}
	if users.Meta["return_type"] != "NSArray" {
		t.Errorf("users return_type = %v, want NSArray (nullable + generic + pointer stripped)", users.Meta["return_type"])
	}
}
