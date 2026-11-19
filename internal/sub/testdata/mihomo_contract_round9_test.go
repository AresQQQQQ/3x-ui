package adapter

import (
	"os"
	"reflect"
	"testing"

	C "github.com/metacubex/mihomo/constant"
	"go.yaml.in/yaml/v3"
)

func TestGhostLinkRound9YAMLContract(t *testing.T) {
	path := os.Getenv("GHOSTLINK_CONTRACT_YAML")
	if path == "" {
		t.Fatal("GHOSTLINK_CONTRACT_YAML is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated YAML: %v", err)
	}
	var document struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse generated YAML: %v", err)
	}
	if len(document.Proxies) != 1 {
		t.Fatalf("generated proxies = %d, want 1", len(document.Proxies))
	}

	proxy, err := ParseProxy(document.Proxies[0])
	if err != nil {
		t.Fatalf("Mihomo ParseProxy: %v", err)
	}
	defer proxy.Close()
	if proxy.Type() != C.GhostLink {
		t.Fatalf("parsed proxy type = %s, want ghostlink", proxy.Type())
	}
	if !proxy.SupportUDP() {
		t.Fatal("parsed GhostLink proxy does not advertise UDP")
	}

	adapterValue := reflect.ValueOf(proxy.Adapter())
	if adapterValue.Kind() != reflect.Pointer || adapterValue.IsNil() {
		t.Fatalf("parsed adapter = %T, want pointer", proxy.Adapter())
	}
	wrapped := adapterValue.Elem().FieldByName("ProxyAdapter")
	if !wrapped.IsValid() || wrapped.IsNil() {
		t.Fatal("parsed adapter has no inner GhostLink")
	}
	element := wrapped.Elem().Elem()
	option := element.FieldByName("option")
	if !option.IsValid() {
		t.Fatal("parsed GhostLink proxy has no option field")
	}
	for field, want := range map[string]string{
		"Name":       "GhostLink-ghost@example.test",
		"Server":     "sub.example.com",
		"ClientID":   "test-client-id-01",
		"Secret":     "0123456789abcdef0123456789abcdef0123456789abcdef",
		"ServerName": "edge.example.com",
		"CAFile":     "",
	} {
		if got := option.FieldByName(field).String(); got != want {
			t.Fatalf("option.%s = %q, want %q", field, got, want)
		}
	}
	if got := option.FieldByName("Port").Int(); got != 24443 {
		t.Fatalf("option.Port = %d, want 24443", got)
	}
	if !option.FieldByName("EnableUDPCapsule").Bool() {
		t.Fatal("enable-udp-capsule did not reach GhostLinkOption")
	}

	manager := element.FieldByName("manager")
	if manager.Kind() != reflect.Pointer || manager.IsNil() {
		t.Fatal("parsed GhostLink proxy has no AutoManager")
	}
	if !manager.Elem().FieldByName("enableH2UDPCapsule").Bool() {
		t.Fatal("GhostLinkOption.EnableUDPCapsule did not reach AutoManager.EnableH2UDPCapsule")
	}
}
