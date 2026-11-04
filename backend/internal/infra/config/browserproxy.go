package config

import (
	"fmt"
	"net"
	"strings"
)

// BrowserProxyConfig is deployment-only, not an account/runtime setting.
type BrowserProxyConfig struct {
	ListenHost    string `yaml:"listenHost"`
	AdvertiseHost string `yaml:"advertiseHost"`
}

func (c BrowserProxyConfig) Validate() error {
	for name, host := range map[string]string{"listenHost": c.ListenHost, "advertiseHost": c.AdvertiseHost} {
		if host == "" {
			continue
		} // local defaults
		if strings.TrimSpace(host) != host || strings.ContainsAny(host, "/@?#[] \t\r\n") {
			return fmt.Errorf("browserProxy.%s 必须为主机名或 IP，不含协议、端口或路径", name)
		}
		ip := net.ParseIP(host)
		if strings.Contains(host, ":") && ip == nil {
			return fmt.Errorf("browserProxy.%s 不能包含端口", name)
		}
		if host == "*" || (ip != nil && (ip.IsUnspecified() || ip.IsMulticast())) {
			return fmt.Errorf("browserProxy.%s 必须指定受信任网络接口，不能使用通配地址", name)
		}
	}
	return nil
}
