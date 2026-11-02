package serviceurl

import "testing"

func TestValidateAllowsPublicHTTPSAndTrustedInternalService(t *testing.T) {
	for _, value := range []string{
		"https://service.example.com/api",
		"http://mint:8788/api",
		"http://localhost:8788/api",
		"http://host.docker.internal:8788/api",
		"http://127.0.0.1:8788/api",
		"http://10.0.0.8:8788/api",
		"https://mint:8788/api",
	} {
		if err := Validate(value); err != nil {
			t.Fatalf("%q rejected: %v", value, err)
		}
	}
}

func TestValidateRejectsUnsafeOrMalformedService(t *testing.T) {
	for _, value := range []string{
		"http://service.example.com/api",
		"https://service.example.com:8443/api",
		"https://user:pass@service.example.com/api",
		"https://service.example.com/api?token=value",
		"https://service.example.com/api#fragment",
		"ftp://mint/api",
		"http://8.8.8.8:8788/api",
		"mint:8788/api",
	} {
		if err := Validate(value); err == nil {
			t.Fatalf("unsafe URL %q accepted", value)
		}
	}
}
