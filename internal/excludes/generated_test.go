package excludes

import (
	"reflect"
	"testing"
)

func TestIsGenerated(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"api/user.pb.go", true},
		{"api/user.pb.cc", true},
		{"api/user.pb.h", true},
		{"api/user.pb.swift", true},
		{"proto/user_pb2.py", true},
		{"proto/user_pb2_grpc.py", true},
		{"x_gen.go", true},
		{"x.gen.go", true},
		{"x_generated.go", true},
		{"x.generated.go", true},
		{"model.g.dart", true},
		{"model.freezed.dart", true},
		{"View.g.cs", true},
		{"View.designer.cs", true},
		{"zz_generated.deepcopy.go", true},
		{"store/mock_store.go", true},
		{"store/store_mock.go", true},
		// Go — additional generators.
		{"store/store_mocks.go", true},
		{"topic.pulsar.go", true},
		{"api/user_grpc.pb.go", true},
		// TS / JS.
		{"src/api.generated.ts", true},
		{"src/api.generated.tsx", true},
		{"src/api.gen.js", true},
		{"src/api.gen.jsx", true},
		{"proto/svc_pb.ts", true},
		{"proto/svc_grpc_pb.js", true},
		{"dist/bundle.min.js", true},
		{"dist/bundle.min.mjs", true},
		// Rust / Python.
		{"src/schema.generated.rs", true},
		{"proto/user_pb2.pyi", true},
		// Dart.
		{"m.pb.dart", true},
		{"m.pbgrpc.dart", true},
		{"m.chopper.dart", true},
		{"m.gr.dart", true},
		{"m.config.dart", true},
		// Java.
		{"pkg/UserServiceGrpc.java", true},
		{"pkg/UserOuterClass.java", true},
		// Windows separators normalise.
		{`api\user.pb.go`, true},
		// Negatives.
		{"api/user.go", false},
		{"store/store.go", false},
		{"genuine.go", false}, // "gen" prefix is not a marker
		{"dist/app.js", false},
		{"src/UserService.java", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsGenerated(c.path); got != c.want {
			t.Errorf("IsGenerated(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestGeneratedPeerPaths(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"api/user.pb.go", []string{"api/user.go"}},
		{"proto/user_pb2.py", []string{"proto/user.py"}},
		{"proto/user_pb2_grpc.py", []string{"proto/user.py"}},
		{"x_gen.go", []string{"x.go"}},
		{"x.generated.go", []string{"x.go"}},
		{"model.freezed.dart", []string{"model.dart"}},
		{"View.designer.cs", []string{"View.cs"}},
		{"store/mock_store.go", []string{"store/store.go"}},
		{"store/store_mock.go", []string{"store/store.go"}},
		// New suffixes derive their hand-written peer.
		{"store/store_mocks.go", []string{"store/store.go"}},
		{"topic.pulsar.go", []string{"topic.go"}},
		{"src/api.generated.ts", []string{"src/api.ts"}},
		{"src/api.generated.tsx", []string{"src/api.tsx"}},
		{"src/api.gen.js", []string{"src/api.js"}},
		{"proto/svc_pb.ts", []string{"proto/svc.ts"}},
		{"proto/svc_grpc_pb.js", []string{"proto/svc.js"}},
		{"dist/bundle.min.js", []string{"dist/bundle.js"}},
		{"src/schema.generated.rs", []string{"src/schema.rs"}},
		{"proto/user_pb2.pyi", []string{"proto/user.py"}},
		{"m.pbgrpc.dart", []string{"m.dart"}},
		{"m.chopper.dart", []string{"m.dart"}},
		{"pkg/UserServiceGrpc.java", []string{"pkg/UserService.java"}},
		{"pkg/UserOuterClass.java", []string{"pkg/User.java"}},
		// No clean peer.
		{"zz_generated.deepcopy.go", nil},
		{"api/user.go", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := GeneratedPeerPaths(c.path)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("GeneratedPeerPaths(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
