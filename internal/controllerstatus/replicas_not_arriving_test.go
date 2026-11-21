package controllerstatus

import (
	"fmt"
	"strings"
	"testing"
)

// The key lists one sorted line per variant, at most 20 then a count of the
// rest, and goes away when nothing is missing.
func TestSetReplicasNotArriving(t *testing.T) {
	p := New(nil, nil, "ctrl")
	p.SetReplicasNotArriving([]string{"ns/b (decode): 1/2", "ns/a (prefill): 0/1"})
	if got := p.Data()[KeyReplicasNotArriving]; got != "ns/a (prefill): 0/1\nns/b (decode): 1/2" {
		t.Fatalf("value = %q", got)
	}

	many := make([]string, 0, 25)
	for i := range 25 {
		many = append(many, fmt.Sprintf("ns/v%02d (both): 0/1", i))
	}
	p.SetReplicasNotArriving(many)
	lines := strings.Split(p.Data()[KeyReplicasNotArriving], "\n")
	if len(lines) != 21 || lines[20] != "+5 more" || lines[0] != many[0] {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}

	p.SetReplicasNotArriving(nil)
	if _, ok := p.Data()[KeyReplicasNotArriving]; ok {
		t.Fatal("the key stays after every variant arrived")
	}
}
