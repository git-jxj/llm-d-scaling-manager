package itl

import (
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// An engine's row of a LeaderWorkerSet group (domain.EngineRows) carries
// only what domain.EngineReading keeps, and its group's identity; every
// other field reads zero. The line reads its rows through this package
// (Observe, Admission, the settle), the saturation analyzer's itlRows (the
// per-token reading) and settlerow.Key, and through the ReplicaMetrics
// methods those call. This test reads, from the type-checked sources, every
// ReplicaMetrics field those paths read, and fails when one is neither kept
// by EngineReading nor taken from the group: a field newly read there would
// otherwise be silently zero on every engine's row.

const modulePath = "github.com/llm-d-incubation/llm-d-scaling-manager/"

// engineIdentity is what an engine's row takes from its group
// (EngineReading.rowOf).
var engineIdentity = []string{"PodName", "VariantName", "Namespace", "ModelID", "Ready", "ReadySince",
	"FromWarmPool", "Revision"}

func TestEngineRows_CarryEveryFieldTheLineReads(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
		packages.NeedTypesInfo | packages.NeedImports}
	paths := []string{modulePath + "internal/signals/itl", modulePath + "internal/domain",
		modulePath + "internal/engines/analyzers/saturation", modulePath + "internal/signals/settle/settlerow"}
	pkgs, err := packages.Load(cfg, paths...)
	require.NoError(t, err)
	require.Len(t, pkgs, len(paths))

	rmType := reflect.TypeFor[domain.ReplicaMetrics]()
	isRM := func(t types.Type) bool {
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		n, ok := t.(*types.Named)
		return ok && n.Obj().Name() == rmType.Name() && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == modulePath+"internal/domain"
	}
	type fn struct {
		decl *ast.FuncDecl
		info *types.Info
	}
	methods := map[string]fn{}
	var roots []fn
	for _, p := range pkgs {
		require.Empty(t, p.Errors, "loading %s", p.PkgPath)
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				switch p.PkgPath {
				case modulePath + "internal/signals/itl":
					roots = append(roots, fn{fd, p.TypesInfo})
				case modulePath + "internal/domain":
					if fd.Recv != nil && len(fd.Recv.List) == 1 && isRM(p.TypesInfo.TypeOf(fd.Recv.List[0].Type)) {
						methods[fd.Name.Name] = fn{fd, p.TypesInfo}
					}
				case modulePath + "internal/engines/analyzers/saturation":
					if fd.Name.Name == "itlRows" {
						roots = append(roots, fn{fd, p.TypesInfo})
					}
				case modulePath + "internal/signals/settle/settlerow":
					if fd.Name.Name == "Key" {
						roots = append(roots, fn{fd, p.TypesInfo})
					}
				}
			}
		}
	}
	require.GreaterOrEqual(t, len(roots), 30, "precondition: the itl package's functions and the two others were found")
	require.Contains(t, methods, "KvUsageForITL", "precondition: the ReplicaMetrics methods were found")

	read := map[string]bool{}
	seen := map[string]bool{}
	var walk func(f fn)
	walk = func(f fn) {
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			s, ok := f.info.Selections[sel]
			if !ok || !isRM(s.Recv()) {
				return true
			}
			switch s.Kind() {
			case types.FieldVal:
				read[sel.Sel.Name] = true
			case types.MethodVal:
				if m, ok := methods[sel.Sel.Name]; ok && !seen[sel.Sel.Name] {
					seen[sel.Sel.Name] = true
					walk(m)
				}
			}
			return true
		})
	}
	for _, r := range roots {
		walk(r)
	}
	require.Contains(t, read, "AvgITL", "precondition: the walk reads the ITL")

	kept := slices.Clone(engineIdentity)
	er := reflect.TypeFor[domain.EngineReading]()
	for i := range er.NumField() {
		kept = append(kept, er.Field(i).Name)
	}
	var missing []string
	for name := range read {
		if !slices.Contains(kept, name) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	assert.Empty(t, missing, "ReplicaMetrics fields the line reads that an engine's row does not carry: "+
		"add them to domain.EngineReading (and EngineReadingOf, rowOf)")
	t.Logf("the line reads %d ReplicaMetrics fields through %d methods", len(read), len(seen))
}
