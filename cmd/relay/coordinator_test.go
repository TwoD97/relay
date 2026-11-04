package main

import (
	"context"
	"strings"
	"testing"
)

func TestCoordinatorRejectsUnsafeIdentity(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 31), strings.Repeat("A", 32), "../../other", strings.Repeat("a", 32) + ";true"} {
		if _, err := coordinatorOptions(id, ""); err == nil {
			t.Fatal("unsafe coordinator identity accepted")
		}
	}
}
func TestCoordinatorPreparationDoesNotMintDesktopToken(t *testing.T) {
	info := controllerInfo{Protocol: 1, Version: version, PID: 123, Address: "http://127.0.0.1:1"}
	launch, err := issueControllerLaunch(context.Background(), uiOptions{NoLaunch: true, StateDir: "/missing-fixture-only"}, info)
	if err != nil || launch.URL != "" || launch.controllerInfo != info {
		t.Fatal("preparation contacted desktop launch route or returned a token", err)
	}
}
