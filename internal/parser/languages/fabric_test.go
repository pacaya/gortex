package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func fabricNode(nodes []*graph.Node) *graph.Node {
	for _, n := range nodes {
		if n.Meta != nil {
			if _, ok := n.Meta["fabric_component"]; ok {
				return n
			}
		}
	}
	return nil
}

func TestTSExtract_FabricComponentSpec(t *testing.T) {
	src := `import type { ViewProps } from 'react-native';
import type { DirectEventHandler } from 'react-native/Libraries/Types/CodegenTypes';
import codegenNativeComponent from 'react-native/Libraries/Utilities/codegenNativeComponent';

export interface NativeProps extends ViewProps {
  color?: string;
  onColorChanged?: DirectEventHandler<{ color: string }>;
}

export default codegenNativeComponent<NativeProps>('RCTColorView');
`
	r, err := NewTypeScriptExtractor().Extract("ColorViewNativeComponent.ts", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	n := fabricNode(r.Nodes)
	if n == nil {
		t.Fatal("no fabric component node emitted")
	}
	if n.Meta["fabric_component"] != "RCTColorView" {
		t.Errorf("fabric_component = %v, want RCTColorView", n.Meta["fabric_component"])
	}
	events, _ := n.Meta["fabric_events"].([]string)
	if len(events) != 1 || events[0] != "onColorChanged" {
		t.Errorf("fabric_events = %v, want [onColorChanged]", n.Meta["fabric_events"])
	}
}

func TestObjCExtract_FabricViewManager(t *testing.T) {
	src := `@implementation RCTColorViewManager

RCT_EXPORT_MODULE()

RCT_EXPORT_VIEW_PROPERTY(color, NSString)
RCT_EXPORT_VIEW_PROPERTY(onColorChanged, RCTDirectEventBlock)

@end
`
	r, err := NewObjCExtractor().Extract("RCTColorViewManager.m", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	n := fabricNode(r.Nodes)
	if n == nil {
		t.Fatal("no fabric view-manager node emitted")
	}
	// Class RCTColorViewManager → component RCTColorView (Manager stripped).
	if n.Meta["fabric_component"] != "RCTColorView" {
		t.Errorf("fabric_component = %v, want RCTColorView", n.Meta["fabric_component"])
	}
	props, _ := n.Meta["fabric_props"].([]string)
	if len(props) != 2 {
		t.Errorf("fabric_props = %v, want 2 props", props)
	}
}

func TestJavaExtract_FabricViewManager(t *testing.T) {
	src := `package com.example;
import com.facebook.react.uimanager.annotations.ReactProp;

public class ColorViewManager extends SimpleViewManager<ColorView> {
    @Override
    public String getName() { return "RCTColorView"; }

    @ReactProp(name = "color")
    public void setColor(ColorView view, String color) {}
}
`
	r, err := NewJavaExtractor().Extract("ColorViewManager.java", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	n := fabricNode(r.Nodes)
	if n == nil {
		t.Fatal("no fabric view-manager node emitted")
	}
	if n.Meta["fabric_component"] != "RCTColorView" {
		t.Errorf("fabric_component = %v, want RCTColorView (from getName)", n.Meta["fabric_component"])
	}
}
