package rollup

// breakCycles makes the open-epic graph a forest before children or counts
// are built. Each node has at most one parent: a walk either ends, joins a
// completed walk, or finds one cycle. Cut the smallest source/ref key in that
// cycle, regardless of map or adapter order. Every other edge stays intact.
func breakCycles(nodes map[string]*node) {
	state := make(map[string]uint8, len(nodes))
	for start := range nodes {
		if state[start] != 0 {
			continue
		}
		var path []string
		k := start
		for k != "" && state[k] == 0 {
			state[k] = 1
			path = append(path, k)
			k = nodes[k].parent
		}
		if k != "" && state[k] == 1 {
			root := k
			for p := nodes[k].parent; p != k; p = nodes[p].parent {
				if p < root {
					root = p
				}
			}
			nodes[root].parent = ""
		}
		for _, k := range path {
			state[k] = 2
		}
	}
}
