package server

import "testing"

func TestIsPublicIPv4(t *testing.T) {
	public := []string{"103.143.11.34", "8.8.8.8", "1.1.1.1", "  103.143.11.34  "}
	for _, s := range public {
		if !IsPublicIPv4(s) {
			t.Errorf("%q 应该算公网地址", s)
		}
	}

	reserved := []string{
		"10.0.0.1", "192.168.1.1", "172.16.0.1", "172.31.255.255", "127.0.0.1",
		"169.254.1.1", "100.64.0.1", "100.127.255.254", "192.0.2.1", "198.51.100.1",
		"203.0.113.1", "0.0.0.0", "224.0.0.1", "240.0.0.1", "198.18.0.1",
		"::1", "fd00::1", "not-an-ip", "", "103.143.11",
	}
	for _, s := range reserved {
		if IsPublicIPv4(s) {
			t.Errorf("%q 不该被当成公网地址", s)
		}
	}
}
