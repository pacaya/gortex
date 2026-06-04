package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// rnMethodMeta returns a map of method-name → (rn_module, rn_method) for
// every method node carrying React Native bridge metadata.
func rnMethodMeta(nodes []*graph.Node) map[string][2]string {
	out := map[string][2]string{}
	for _, n := range nodes {
		if n.Kind != graph.KindMethod || n.Meta == nil {
			continue
		}
		mod, _ := n.Meta["rn_module"].(string)
		meth, _ := n.Meta["rn_method"].(string)
		if meth == "" {
			continue
		}
		out[n.Name] = [2]string{mod, meth}
	}
	return out
}

func TestObjCExtract_RNExports(t *testing.T) {
	src := `#import "RCTBridgeModule.h"

@implementation CalendarModule

RCT_EXPORT_MODULE();

RCT_EXPORT_METHOD(addEvent:(NSString *)name location:(NSString *)location)
{
}

RCT_REMAP_METHOD(getThing, fetchThingWithResolver:(RCTPromiseResolveBlock)resolve rejecter:(RCTPromiseRejectBlock)reject)
{
}

@end
`
	r, err := NewObjCExtractor().Extract("CalendarModule.m", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	meta := rnMethodMeta(r.Nodes)
	// RCT_EXPORT_METHOD: selector node "addEvent:location:", JS name "addEvent".
	if got, ok := meta["addEvent:location:"]; !ok || got != [2]string{"CalendarModule", "addEvent"} {
		t.Errorf("addEvent export = %v (ok=%v), want module CalendarModule method addEvent", meta["addEvent:location:"], ok)
	}
	// RCT_REMAP_METHOD: explicit JS name "getThing", native selector
	// "fetchThingWithResolver:rejecter:".
	if got, ok := meta["fetchThingWithResolver:rejecter:"]; !ok || got != [2]string{"CalendarModule", "getThing"} {
		t.Errorf("getThing remap = %v (ok=%v), want module CalendarModule method getThing", meta["fetchThingWithResolver:rejecter:"], ok)
	}
}

func TestObjCExtract_RNModuleExplicitName(t *testing.T) {
	src := `@implementation Impl
RCT_EXPORT_MODULE(MyJSName);
RCT_EXPORT_METHOD(doStuff:(NSString *)x) {}
@end
`
	r, err := NewObjCExtractor().Extract("Impl.m", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	meta := rnMethodMeta(r.Nodes)
	if got := meta["doStuff:"]; got != [2]string{"MyJSName", "doStuff"} {
		t.Errorf("doStuff = %v, want module MyJSName method doStuff", got)
	}
}

func TestJavaExtract_RNReactMethod(t *testing.T) {
	src := `package com.example;
import com.facebook.react.bridge.ReactMethod;
import com.facebook.react.module.annotations.ReactModule;

@ReactModule(name = "Calendar")
public class CalendarModule extends ReactContextBaseJavaModule {
    @Override
    public String getName() { return "Calendar"; }

    @ReactMethod
    public void createEvent(String name, String location) {}

    public void notExported() {}
}
`
	r, err := NewJavaExtractor().Extract("CalendarModule.java", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	meta := rnMethodMeta(r.Nodes)
	if got, ok := meta["createEvent"]; !ok || got != [2]string{"Calendar", "createEvent"} {
		t.Errorf("createEvent = %v (ok=%v), want module Calendar (the @ReactModule override) method createEvent", meta["createEvent"], ok)
	}
	if _, ok := meta["notExported"]; ok {
		t.Errorf("notExported must not carry RN metadata")
	}
}

// rnCallTargets returns the To-ends of every rn.native placeholder call
// edge emitted by a JS/TS extractor.
func rnCallTargets(edges []*graph.Edge) []string {
	var out []string
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == rnNativeVia {
				out = append(out, e.To)
			}
		}
	}
	return out
}

func TestJSExtract_RNNativeCalls(t *testing.T) {
	cases := map[string]string{
		"direct": `import { NativeModules } from 'react-native';
function go() { NativeModules.Calendar.createEvent('b', 'home'); }`,
		"bound_var": `import { NativeModules } from 'react-native';
const Cal = NativeModules.Calendar;
function go() { Cal.createEvent('b', 'home'); }`,
		"require_native": `const Cal = requireNativeModule('Calendar');
function go() { Cal.createEvent('b'); }`,
		"turbomodule": `const Cal = TurboModuleRegistry.getEnforcing('Calendar');
function go() { Cal.createEvent('b'); }`,
		"destructure": `import { NativeModules } from 'react-native';
const { Calendar } = NativeModules;
function go() { Calendar.createEvent('b'); }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := NewJavaScriptExtractor().Extract("app.js", []byte(src))
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			targets := rnCallTargets(r.Edges)
			want := "unresolved::rn::Calendar::createEvent"
			found := false
			for _, tg := range targets {
				if tg == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: rn native call targets = %v, want %q", name, targets, want)
			}
		})
	}
}
