package controllerstatus

import (
	"testing"
)

// The status keys of the install from the one release manifest: the managed
// and refused namespaces, the preflight checks with ready, and KEDA's
// connection.
func TestInstallStatusKeys(t *testing.T) {
	p := New(nil, nil, "scaling-manager-system")

	p.SetNamespaces([]string{"team-b", "team-a"}, map[string]string{"tenant-x": "it is not labelled", "team-c": "not admitted"})
	d := p.Data()
	if got := d[KeyNamespacesManaged]; got != "team-a,team-b" {
		t.Errorf("%s %q", KeyNamespacesManaged, got)
	}
	if got := d[KeyNamespacesRefused]; got != "team-c: not admitted\ntenant-x: it is not labelled" {
		t.Errorf("%s %q", KeyNamespacesRefused, got)
	}
	p.SetNamespaces(nil, nil)
	d = p.Data()
	if got, ok := d[KeyNamespacesManaged]; !ok || got != "" {
		t.Errorf("no managed namespace: %s %q, present %v; it must say none, not vanish", KeyNamespacesManaged, got, ok)
	}
	if _, ok := d[KeyNamespacesRefused]; ok {
		t.Errorf("%s present with none refused", KeyNamespacesRefused)
	}

	p.SetPreflight(map[string]string{"keda.crd": "ok: found", "kueue": "warn: x"}, "ok")
	p.SetPreflight(map[string]string{"keda.crd": "fail: gone"}, "not ready: keda.crd")
	d = p.Data()
	if d[KeyPreflightPrefix+"keda.crd"] != "fail: gone" || d[KeyReady] != "not ready: keda.crd" {
		t.Errorf("preflight keys %q ready %q", d[KeyPreflightPrefix+"keda.crd"], d[KeyReady])
	}
	if _, ok := d[KeyPreflightPrefix+"kueue"]; ok {
		t.Error("a check no longer reported kept its key")
	}

	p.SetKEDAConnection("current", false)
	d = p.Data()
	if d[KeyKEDAConnection] != "current" || d[KeyKEDACopyStale] != "false" {
		t.Errorf("kedaConnection %q kedaCopyStale %q", d[KeyKEDAConnection], d[KeyKEDACopyStale])
	}

	var nilP *Publisher
	nilP.SetNamespaces([]string{"a"}, nil)
	nilP.SetPreflight(nil, "ok")
	nilP.SetKEDAConnection("none", true)
}
