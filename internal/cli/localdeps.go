package cli

import (
	"fmt"
	"strings"

	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/system"
)

// localBatches uses stable Kahn layers: unrelated builds share a transaction,
// while every pending own dependency is installed before its consumer builds.
// Only the user calls this: root must never evaluate a PKGBUILD.
func (a *app) localBatches(steps []plan.Step) ([][]plan.Step, error) {
	pending := map[string]bool{}
	info := map[string]system.Package{}
	for _, s := range steps {
		pending[s.Names[0]] = true
	}
	for _, s := range steps {
		n := s.Names[0]
		p, err := system.SrcInfo(a.modules[s.Module].RecipeDir(n), n)
		if err != nil {
			return nil, err
		}
		info[n] = p
	}
	var batches [][]plan.Step
	for len(pending) > 0 {
		var batch []plan.Step
		for _, s := range steps {
			n := s.Names[0]
			if !pending[n] {
				continue
			}
			p := info[n]
			deps := append(append(append([]string{}, p.Depends...), p.MakeDepends...), p.CheckDepends...)
			ready := true
			for _, dep := range deps {
				name := dep
				if i := strings.IndexAny(name, "<>="); i >= 0 {
					name = name[:i]
				}
				if pending[name] {
					ready = false
					break
				}
			}
			if ready {
				batch = append(batch, s)
			}
		}
		if len(batch) == 0 {
			var names []string
			for _, s := range steps {
				if pending[s.Names[0]] {
					names = append(names, s.Names[0])
				}
			}
			return nil, fmt.Errorf("own build dependency cycle (or builds blocked by it): %s", strings.Join(names, ", "))
		}
		for _, s := range batch {
			delete(pending, s.Names[0])
		}
		batches = append(batches, batch)
	}
	return batches, nil
}
