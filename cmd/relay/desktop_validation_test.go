package main

import (
	"strings"
	"testing"
)

func TestDesktopLaunchRejectsUntrustedDestinations(t *testing.T) {
	base := desktopLaunch{controllerInfo: controllerInfo{Address: "http://127.0.0.1:43210", Version: version, PID: 100, Protocol: controllerProtocol}, URL: "http://127.0.0.1:43210/auth?token=" + strings.Repeat("ab", 24)}
	if err := validateDesktopLaunch(base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*desktopLaunch){
		"remote origin":     func(l *desktopLaunch) { l.Address = "http://192.0.2.1:43210" },
		"DNS hostname":      func(l *desktopLaunch) { l.Address = "http://localhost:43210" },
		"userinfo":          func(l *desktopLaunch) { l.URL = strings.Replace(l.URL, "//", "//user@", 1) },
		"different port":    func(l *desktopLaunch) { l.URL = strings.Replace(l.URL, "43210", "43211", 1) },
		"script scheme":     func(l *desktopLaunch) { l.URL = "javascript:alert(1)" },
		"fragment":          func(l *desktopLaunch) { l.URL += "#fragment" },
		"redirect query":    func(l *desktopLaunch) { l.URL += "&next=https://example.org" },
		"duplicate token":   func(l *desktopLaunch) { l.URL += "&token=other" },
		"encoded endpoint":  func(l *desktopLaunch) { l.URL = strings.Replace(l.URL, "/auth", "/%61uth", 1) },
		"invalid token":     func(l *desktopLaunch) { l.URL = "http://127.0.0.1:43210/auth?token=short" },
		"protocol mismatch": func(l *desktopLaunch) { l.Protocol++ },
		"version mismatch":  func(l *desktopLaunch) { l.Version += ".other" },
		"invalid pid":       func(l *desktopLaunch) { l.PID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			change(&candidate)
			if err := validateDesktopLaunch(candidate); err == nil {
				t.Fatal("unsafe controller response accepted")
			}
		})
	}
}
