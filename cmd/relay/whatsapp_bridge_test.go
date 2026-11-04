package main

import (
	"strings"
	"testing"
)

func TestWhatsAppBusinessBridgeFlagsFailBeforeNetwork(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "--config"},
		{[]string{"--config", "unused", "--listen", "0.0.0.0:8787"}, "loopback"},
		{[]string{"--config", "unused", "--listen", "localhost:8787"}, "literal loopback"},
		{[]string{"--config", "unused", "--tls-cert", "unused"}, "both TLS"},
		{[]string{"--config", "unused", "--listen", "invalid"}, "listen address"},
	} {
		if err := whatsappBridge(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%v: %v", test.args, err)
		}
	}
}
