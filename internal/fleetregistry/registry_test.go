package fleetregistry

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func fixture(source string, revision uint64) Snapshot {
	return Snapshot{Format: 1, ControllerID: strings.Repeat(source, 48), Revision: revision, IssuedAt: testNow, ExpiresAt: testNow.Add(Lifetime), Hosts: []Host{{ID: "docker", Name: "Mynet Docker Live", ConfiguredTarget: "desktop-only-alias", Target: "operator@10.8.0.5", Port: 2222}}}
}

func TestResolveExactNamesAndControllerConflicts(t *testing.T) {
	a := fixture("a", 1)
	listing := Listing{Sources: []Source{{Snapshot: a}}}
	for _, name := range []string{"Mynet Docker Live", "docker"} {
		got, err := listing.Resolve(name, "", testNow)
		if err != nil || got.Target != "operator@10.8.0.5" || got.Port != 2222 || got.Self {
			t.Fatalf("exact name: %+v %v", got, err)
		}
	}
	for _, name := range []string{"mynet docker live", "Mynet Docker", " Mynet Docker Live ", "desktop-only-alias"} {
		if _, err := listing.Resolve(name, "", testNow); err == nil {
			t.Fatalf("guessed a name: %q", name)
		}
	}
	b := fixture("b", 1)
	b.Hosts[0].ID = "other-controller-id"
	listing.Sources = append(listing.Sources, Source{Snapshot: b})
	if got, err := listing.Resolve("Mynet Docker Live", "", testNow); err != nil || len(got.ControllerIDs) != 2 {
		t.Fatalf("identical endpoint did not deduplicate: %+v %v", got, err)
	}
	listing.Sources[1].Hosts[0].Port = 22
	if _, err := listing.Resolve("Mynet Docker Live", "", testNow); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("conflicting controllers were accepted", err)
	}
	if got, err := listing.Resolve("Mynet Docker Live", a.ControllerID, testNow); err != nil || got.Port != 2222 {
		t.Fatal("explicit source failed", got, err)
	}
	listing.Sources[1].Hosts[0].Port = 2222
	listing.Sources[1].Hosts[0].ID = "Mynet Docker Live"
	listing.Sources[1].Hosts[0].Name = "Different machine"
	if _, err := listing.Resolve("Mynet Docker Live", "", testNow); err == nil {
		t.Fatal("name/ID collision was accepted")
	}
	listing = Listing{Sources: []Source{{Snapshot: a}}}
	listing.Sources[0].Hosts = append(listing.Sources[0].Hosts, Host{ID: "second", Name: "Mynet Docker Live", ConfiguredTarget: "another-alias", Target: "operator@10.8.0.5", Port: 2222})
	if _, err := listing.Resolve("Mynet Docker Live", "", testNow); err == nil {
		t.Fatal("duplicate names in one controller were guessed")
	}
	// A third matching record must not hide an ambiguity within the second
	// controller behind the first controller's equivalent endpoint.
	listing.Sources = append([]Source{{Snapshot: fixture("b", 1)}}, listing.Sources...)
	if _, err := listing.Resolve("Mynet Docker Live", "", testNow); err == nil {
		t.Fatal("a third same-endpoint match hid a duplicate name")
	}
}

func TestResolveFreshnessAndSelfDoNotGrantSSHAuthorization(t *testing.T) {
	s := fixture("a", 1)
	s.Hosts[0].Target, s.Hosts[0].Port, s.Hosts[0].Unavailable = "", 0, "Loopback endpoint cannot be shared"
	listing := Listing{Sources: []Source{{Snapshot: s}}}
	if _, err := listing.Resolve("docker", "", testNow); err == nil || !strings.Contains(err.Error(), "no portable SSH endpoint") {
		t.Fatal("unresolved endpoint accepted", err)
	}
	listing.Sources[0].SelfHostID = "docker"
	if got, err := listing.Resolve("docker", "", testNow); err != nil || !got.Self || got.Target != "" {
		t.Fatal("self did not resolve locally", got, err)
	}
	if _, err := listing.Resolve("docker", "", testNow.Add(Lifetime)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatal("expired name accepted", err)
	}
	listing.Sources[0].IssuedAt = testNow.Add(3 * time.Minute)
	if _, err := listing.Resolve("docker", "", testNow); err == nil {
		t.Fatal("future controller clock accepted")
	}
}

func TestFleetDecoderRejectsUnexpectedFieldsAndBounds(t *testing.T) {
	for _, input := range []string{`{"format":1,"password":"never accepted"}`, `{}` + `{}`, strings.Repeat("x", MaxBytes+1)} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatal("invalid registry accepted")
		}
	}
	if _, err := DirectoryForSocket("relative/run/daemon.sock"); err == nil {
		t.Fatal("relative runtime accepted")
	}
	if _, err := DirectoryForSocket("/tmp/not-the-runtime.sock"); err == nil {
		t.Fatal("unrelated socket accepted")
	}
}
