package pbx

import (
	"os"
	"testing"
)

func TestParsePeers(t *testing.T) {
	html, err := os.ReadFile("testdata/peers.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	peers, err := ParsePeers(string(html))
	if err != nil {
		t.Fatalf("ParsePeers failed: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	if peers[0].Extension != "100" || peers[0].CallerID != "MARIO ROSSI" {
		t.Errorf("peers[0] = %+v, unexpected", peers[0])
	}
	if peers[1].Extension != "200" || peers[1].Status != "1" {
		t.Errorf("peers[1] = %+v, unexpected", peers[1])
	}
}

func TestParsePeersMissingVar(t *testing.T) {
	_, err := ParsePeers("<html><body>nothing here</body></html>")
	if err == nil {
		t.Fatal("expected error when infopeers var is missing")
	}
}

func TestParseCallGroups(t *testing.T) {
	html, err := os.ReadFile("testdata/callgroups.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	groups := ParseCallGroups(string(html))
	if len(groups) != 2 {
		t.Fatalf("got %d call groups, want 2", len(groups))
	}

	g0 := groups[0]
	if g0.Name != "Ufficio Test" || g0.Extension != "495" {
		t.Errorf("groups[0] = %+v, unexpected", g0)
	}
	if len(g0.Members) != 2 || g0.Members[0] != "740" || g0.Members[1] != "741" {
		t.Errorf("groups[0].Members = %v, want [740 741]", g0.Members)
	}
	if !g0.Enabled {
		t.Error("groups[0].Enabled = false, want true")
	}

	g1 := groups[1]
	if g1.Enabled {
		t.Error("groups[1].Enabled = true, want false (off.gif)")
	}
	if len(g1.Members) != 1 || g1.Members[0] != "742" {
		t.Errorf("groups[1].Members = %v, want [742]", g1.Members)
	}
}
