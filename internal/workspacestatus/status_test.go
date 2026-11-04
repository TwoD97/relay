package workspacestatus

import (
	"strings"
	"testing"
	"time"
)

func TestStatusUsesRecordedTasksAndEscapesUntrustedContent(t *testing.T) {
	input := Input{Live: true, Title: "Status", CoordinatorConfigured: true, CoordinatorEnabled: true, CoordinatorStatus: "running", UpdatedAt: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC), Tasks: []Task{{Title: `<script>alert(1)</script>`, Detail: `<img src=x onerror=alert(2)>`, Status: "doing", HostID: `<b>host</b>`}, {Title: "Check result", Status: "todo"}, {Title: "Missing dependency", Status: "blocked"}, {Title: "Verified fixture", Status: "done"}}}
	html, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Process running", "2026-11-04 09:00:00 UTC", "&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img src=x onerror=alert(2)&gt;", "4 tasks recorded", "Verified fixture"} {
		if !strings.Contains(html, expected) {
			t.Fatalf("missing recorded fact %q", expected)
		}
	}
	for _, forbidden := range []string{"<script>", "<img src=x", "<b>host</b>", "<iframe", "<form", "https://"} {
		if strings.Contains(html, forbidden) {
			t.Fatal("active/unescaped content", forbidden)
		}
	}
	input.Tasks = nil
	html, err = Render(input)
	if err != nil || !strings.Contains(html, "Ready for your next task") || strings.Contains(html, "1 tasks recorded") {
		t.Fatal("process state manufactured tasks")
	}
	if !strings.Contains(html, "light-dark(") || strings.Contains(html, "width:1200") || !strings.Contains(html, "minmax(min(100%,320px)") {
		t.Fatal("template is not responsive/theme-aware")
	}
}

func TestStatusBoundsTaskRowsWithoutLosingTotalCounts(t *testing.T) {
	input := Input{Live: true, Title: strings.Repeat(`"`, 160)}
	for i := 0; i < 200; i++ {
		input.Tasks = append(input.Tasks, Task{Title: strings.Repeat(`"<&`, 160), Detail: strings.Repeat(`"<&`, 1000), HostID: strings.Repeat(`"`, 128), SessionID: strings.Repeat("f", 32), Status: "doing"})
	}
	html, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(html) > 128<<10 || strings.Count(html, "<li>") != 40 || !strings.Contains(html, "Showing 40 of 200 task records") {
		t.Fatal("unbounded or misleading status", len(html))
	}
	input.Tasks = append(input.Tasks, Task{})
	if _, err := Render(input); err == nil {
		t.Fatal("oversized taskboard accepted")
	}
}

func TestStandaloneStatusTemplateHasNoFalseLiveClaim(t *testing.T) {
	html, err := Starter()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "Project status template") || strings.Contains(html, "updates automatically") || strings.Contains(html, "<h2>Coordinator</h2>") {
		t.Fatal("static starter claimed live data")
	}
}
