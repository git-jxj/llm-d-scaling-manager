package capacity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// Every key the parsers write to MemoryLayout is in memoryLayoutFlags, so a
// flag added to MemoryLayout without its value shape fails here rather than
// in a reader that refuses what it does not know (the recording export).
// The keys are read from the addLayoutPart calls in the package's own source:
// a string literal, a package constant, or the case values of the switch
// case the call sits in.
func TestEveryMemoryLayoutKeyIsInTheTable(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		parsed = append(parsed, f)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[n.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	// keyOf is the key an expression names: a string literal or a constant.
	keyOf := func(e ast.Expr) (string, bool) {
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				s, err := strconv.Unquote(e.Value)
				return s, err == nil
			}
		case *ast.Ident:
			s, ok := consts[e.Name]
			return s, ok
		}
		return "", false
	}
	isAdd := func(n ast.Node) (*ast.CallExpr, bool) {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return nil, false
		}
		fn, ok := call.Fun.(*ast.Ident)
		return call, ok && fn.Name == "addLayoutPart"
	}

	keys := map[string]bool{}
	covered := map[*ast.CallExpr]bool{}
	calls := 0
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, stmt := range cc.Body {
				ast.Inspect(stmt, func(n ast.Node) bool {
					if _, nested := n.(*ast.CaseClause); nested {
						return false // a nested case takes its own values
					}
					call, ok := isAdd(n)
					if !ok {
						return true
					}
					if _, named := keyOf(call.Args[1]); named {
						return true
					}
					covered[call] = true
					for _, e := range cc.List {
						k, ok := keyOf(e)
						require.True(t, ok, "a case value of an addLayoutPart switch at %s is not a constant", fset.Position(e.Pos()))
						keys[k] = true
					}
					return true
				})
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := isAdd(n)
			if !ok {
				return true
			}
			calls++
			if k, named := keyOf(call.Args[1]); named {
				keys[k] = true
			} else if !covered[call] {
				t.Errorf("addLayoutPart at %s takes a key this test cannot read", fset.Position(call.Pos()))
			}
			return true
		})
	}
	require.GreaterOrEqual(t, calls, 6, "precondition: the parsers' addLayoutPart calls were found")
	require.GreaterOrEqual(t, len(keys), len(memoryLayoutFlags), "precondition: every flag's key was read")
	for k := range keys {
		assert.Contains(t, memoryLayoutFlags, k, "MemoryLayout key %q has no value shape in memoryLayoutFlags", k)
	}
}

// What both parsers write to MemoryLayout is known, flag by flag.
func TestKnownMemoryLayout_HoldsWhatTheParsersWrite(t *testing.T) {
	vllm := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "org/m",
		"--num-redundant-experts", "32",
		"--kv-cache-memory-bytes", "40G",
		"--enable-eplb",
		"--eplb-config", `{"window_size": 1000, "num_redundant_experts": 32, "use_async": true}`,
		"--all2all-backend", "deepep_low_latency",
	}, corev1.EnvVar{Name: "VLLM_ALL2ALL_BACKEND", Value: "deepep_high_throughput"})))
	sglang := ParseEngineArgs(inferenceengine.EngineSGLang, deploymentWithContainers(corev1.Container{
		Name: "sglang", Image: "lmsysorg/sglang:v0.5", Command: []string{
			"python", "-m", "sglang.launch_server", "--model-path", "org/m",
			"--enable-dp-attention", "--moe-a2a-backend", "deepep",
		}}))
	written := map[string]bool{}
	for _, layout := range []string{vllm.MemoryLayout, sglang.MemoryLayout} {
		require.NotEmpty(t, layout, "precondition: the parser wrote a layout")
		assert.True(t, KnownMemoryLayout(layout), "%q is not known", layout)
		for _, part := range strings.Split(layout, ";") {
			k, _, _ := strings.Cut(part, "=")
			written[k] = true
		}
	}
	for k := range memoryLayoutFlags {
		assert.True(t, written[k], "no parser run above wrote %q: add it, so its value shape is held", k)
	}
	assert.True(t, KnownMemoryLayout(""), "no layout is known")
	assert.True(t, KnownMemoryLayout("kv_cache_memory_bytes=42949672960"), "a size in bytes")
}

// A layout whose key or value could be a name is not known: a key no parser
// writes, a backend the engines do not define, text where a number goes, and
// a string anywhere in the EPLB configuration.
func TestKnownMemoryLayout_RefusesWhatCouldBeAName(t *testing.T) {
	for _, layout := range []string{
		"payroll=true",
		"all2all_backend=payroll",
		"env.VLLM_ALL2ALL_BACKEND=payroll-team",
		"moe_a2a_backend=payroll",
		"kv_cache_memory_bytes=payroll",
		"kv_cache_memory_bytes=40payroll",
		"num_redundant_experts=payroll",
		"num_redundant_experts=-3",
		"enable_eplb=payroll",
		"enable_dp_attention=false",
		`eplb_config={"window_size":"payroll"}`,
		`eplb_config={"payroll":1}`,
		`eplb_config={"window_size":{"payroll":1}}`,
		"eplb_config=payroll",
		"all2all_backend=deepep_low_latency;payroll",
		"all2all_backend=deepep_low_latency;",
	} {
		assert.False(t, KnownMemoryLayout(layout), "%q is known", layout)
	}
}
