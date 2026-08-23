package main

import (
	"strings"
	"testing"
)

func TestValidComponent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"main", true},
		{"acme", true},
		{"feature-x", true},
		{"my.repo", true}, // dots allowed inside components
		{"v1.2", true},    // version-ish names
		{"a.b.c", true},   // multiple dots fine
		{"Feature_X", true},
		{"double--dash", true},
		{"a", true},
		{"", false},
		{"-lead", false},                  // leading dash
		{"has:colon", false},              // reserved separator
		{"dot..dot", false},               // traversal / jj rule
		{"trailing.", false},              // jj rule
		{"name.lock", false},              // jj/git rule
		{"has space", false},              // charset
		{"has/slash", false},              // charset
		{"中文", false},                     // charset
		{strings.Repeat("a", 128), true},  // max length
		{strings.Repeat("a", 129), false}, // too long
		{"0start", true},
	}
	for _, c := range cases {
		if got := validComponent(c.in); got != c.want {
			t.Errorf("validComponent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSessionNameRoundtrip(t *testing.T) {
	org, repo, bm := "acme", "my.repo", "feature-1.2"
	name := sessionName(org, repo, bm)
	if name != "acme:my.repo:feature-1.2" {
		t.Fatalf("sessionName = %q", name)
	}
	o2, r2, b2, ok := parseSessionName(name)
	if !ok || o2 != org || r2 != repo || b2 != bm {
		t.Fatalf("parseSessionName(%q) = %q,%q,%q,%v", name, o2, r2, b2, ok)
	}
}

func TestParseSessionNameRejectsAmbiguity(t *testing.T) {
	for _, bad := range []string{
		"", "onlyone", "a:b", "a:b:c:d", "no-trailing:",
	} {
		if _, _, _, ok := parseSessionName(bad); ok {
			t.Errorf("parseSessionName(%q) should fail", bad)
		}
	}
}

func TestDerivationIsInjectiveForValidComponents(t *testing.T) {
	// Valid components never contain `:`, so derivation is reversible and
	// distinct triples can never produce the same session name — including
	// triples whose components contain dots.
	triples := [][3]string{
		{"a", "b", "c"}, {"a-b", "b", "c"}, {"a", "b-c", "d"}, {"a", "b", "c-d"},
		{"acme", "my.repo", "feat"}, {"acme", "my", "repo.feat-adjacent"},
		{"x", "a.b", "c"}, {"x", "a", "b.c"}, {"x.y", "a", "b"},
	}
	seen := map[string][3]string{}
	for _, tr := range triples {
		for _, part := range tr {
			if !validComponent(part) {
				t.Fatalf("%q unexpectedly invalid", part)
			}
		}
		name := sessionName(tr[0], tr[1], tr[2])
		if prev, dup := seen[name]; dup {
			t.Fatalf("collision: %v and %v both → %q", prev, tr, name)
		}
		seen[name] = tr
		o, r, b, ok := parseSessionName(name)
		if !ok || o != tr[0] || r != tr[1] || b != tr[2] {
			t.Fatalf("roundtrip failed for %v → %q", tr, name)
		}
	}
}
