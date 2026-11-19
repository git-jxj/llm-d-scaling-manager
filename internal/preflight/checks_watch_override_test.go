package preflight

import (
	"strings"
	"testing"
)

// TestWatchNamespaceOverride: smt install sets WatchNamespaceEnv on the Job
// when it is about to apply the controller with another list. An admin who
// took team-b out of the list revoked the controller's Role there, and the
// live Deployment still names it until the install applies the new one: the
// checks read the list the install applies, so the install that drops team-b
// is not stopped by it. Without the variable they read the Deployment, as
// before.
func TestWatchNamespaceOverride(t *testing.T) {
	setup := func(t *testing.T, override string) Report {
		f := newFixture(t)
		watchBoth(f)
		f.sar = deny("list", "pods", "team-b")
		f.watchNamespace = override
		return f.run(t)
	}
	t.Run("the live list, team-b revoked", func(t *testing.T) {
		rep := setup(t, "")
		if r := only(t, rep, "rbac.controller"); r.Status != Fail || !strings.Contains(r.Detail, "team-b") {
			t.Errorf("rbac.controller: got %s %q, want a FAIL naming team-b", r.Status, r.Detail)
		}
		if r := only(t, rep, "rbac.namespaces"); r.Status != Fail {
			t.Errorf("rbac.namespaces: got %s %q, want FAIL", r.Status, r.Detail)
		}
	})
	t.Run("the list the install applies", func(t *testing.T) {
		rep := setup(t, " "+testNS+" ")
		for _, name := range []string{"rbac.controller", "rbac.namespaces"} {
			if r := only(t, rep, name); r.Status == Fail {
				t.Errorf("%s: got FAIL %q for a namespace the install drops", name, r.Detail)
			}
		}
	})
	t.Run("a watchNamespaces setting left from before cannot keep team-b", func(t *testing.T) {
		f := newFixture(t)
		watchBoth(f)
		f.settings[keyWatchNamespaces] = testNS + ",team-b"
		f.watchNamespace = testNS
		s := &state{env: f.env(t)}
		if nss, named := s.managed(); !named || strings.Join(nss, ",") != testNS {
			t.Errorf("managed %v (named %v), want only %s", nss, named, testNS)
		}
	})
	t.Run("cluster scope ignores it", func(t *testing.T) {
		f := newFixture(t)
		f.scope = ScopeCluster
		f.watchNamespace = testNS
		s := &state{env: f.env(t)}
		if nss, named := s.managed(); named {
			t.Errorf("cluster scope: managed %v, want every namespace", nss)
		}
	})
}
