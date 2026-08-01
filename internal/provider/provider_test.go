package provider

import "testing"

func TestValid(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"openai", "sub2api", "provider_1.test", "a-b"} {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false", name)
		}
	}
	for _, name := range []string{"", "provider name", "provider/name", "provider\nname", "提供商"} {
		if Valid(name) {
			t.Errorf("Valid(%q) = true", name)
		}
	}
}
