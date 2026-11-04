package config

import "testing"

func TestBrowserProxyDeploymentConfig(t *testing.T) {
	for _, host := range []string{"", "localhost", "127.0.0.1", "::1", "grok2api-browser-relay", "192.168.10.3"} {
		if err := (BrowserProxyConfig{ListenHost: host, AdvertiseHost: host}).Validate(); err != nil {
			t.Fatalf("%q: %v", host, err)
		}
	}
	for _, host := range []string{"0.0.0.0", "::", "*", "224.0.0.1", "http://backend", "backend:8080", "user@backend", " backend", "backend/path", "[::1]"} {
		if err := (BrowserProxyConfig{ListenHost: host}).Validate(); err == nil {
			t.Fatalf("accepted listen %q", host)
		}
		if err := (BrowserProxyConfig{AdvertiseHost: host}).Validate(); err == nil {
			t.Fatalf("accepted advertise %q", host)
		}
	}
	t.Setenv(DatabaseURLEnv, "")
	t.Setenv(BrowserProxyListenHostEnv, "grok2api-browser-relay")
	t.Setenv(BrowserProxyAdvertiseHostEnv, "grok2api-browser-relay")
	cfg := defaultConfig()
	if err := applyEnvironmentOverrides(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserProxy.ListenHost != "grok2api-browser-relay" || cfg.BrowserProxy.AdvertiseHost != "grok2api-browser-relay" {
		t.Fatal(cfg.BrowserProxy)
	}
	t.Setenv(BrowserProxyListenHostEnv, "")
	t.Setenv(BrowserProxyAdvertiseHostEnv, "")
	if err := applyEnvironmentOverrides(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserProxy.ListenHost != "grok2api-browser-relay" {
		t.Fatal("empty override replaced YAML")
	}
}
