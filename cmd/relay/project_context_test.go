package main

import (
	"strings"
	"testing"
)

func TestProjectContextStdinRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, input := range []string{`null`, `{}`, `{"path":"/tmp","unknown":true}`, `{"path":"/tmp"} {}`, `{"path":"/tmp"} garbage`, `{"path":"relative"}`, `{"path":"/tmp/../other"}`, strings.Repeat(" ", 32<<10) + `{}`} {
		if _, err := readProjectContextRequest(strings.NewReader(input)); err == nil {
			t.Errorf("accepted malformed request %.100q", input)
		}
	}
	got, err := readProjectContextRequest(strings.NewReader(`{"path":"/tmp/a '$() project"}`))
	if err != nil || got.Path != "/tmp/a '$() project" {
		t.Fatal(got, err)
	}
}
