package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func springListenerNode(nodes []*graph.Node, methodName string) *graph.Node {
	for _, n := range nodes {
		if n.Name == methodName && n.Meta != nil && n.Meta["spring_listener_type"] != nil {
			return n
		}
	}
	return nil
}

func springPublishPlaceholder(edges []*graph.Edge, evType string) *graph.Edge {
	for _, e := range edges {
		if e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "spring-event" {
			continue
		}
		if t, _ := e.Meta["spring_event_type"].(string); t == evType {
			return e
		}
	}
	return nil
}

func TestSpringEvents_TagsListenersAndPublishers(t *testing.T) {
	src := `package com.x;
class OrderPlaced {}
class OrderService {
  private ApplicationEventPublisher publisher;
  void place(int id) {
    publisher.publishEvent(new OrderPlaced(id));
  }
}
class MailListener {
  @EventListener
  void on(OrderPlaced e) {}
}
class AuditListener implements ApplicationListener<OrderPlaced> {
  public void onApplicationEvent(OrderPlaced e) {}
}
`
	res, err := NewJavaExtractor().Extract("App.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	on := springListenerNode(res.Nodes, "on")
	if on == nil {
		t.Fatalf("@EventListener method not tagged")
	}
	if ty, _ := on.Meta["spring_listener_type"].(string); ty != "OrderPlaced" {
		t.Errorf("listener type = %q (want OrderPlaced)", ty)
	}
	if springListenerNode(res.Nodes, "onApplicationEvent") == nil {
		t.Errorf("ApplicationListener.onApplicationEvent not tagged")
	}

	ph := springPublishPlaceholder(res.Edges, "OrderPlaced")
	if ph == nil {
		t.Fatalf("no publishEvent placeholder")
	}
	if ph.From != "App.java::OrderService.place" {
		t.Errorf("placeholder From = %q (want App.java::OrderService.place)", ph.From)
	}
}

func TestSpringEvents_NonEventListenerNotTagged(t *testing.T) {
	src := `package com.x;
class Svc {
  @Override
  void on(String e) {}
}
`
	res, err := NewJavaExtractor().Extract("Svc.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if springListenerNode(res.Nodes, "on") != nil {
		t.Errorf("a non-@EventListener method must not be tagged")
	}
}
