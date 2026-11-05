package checkout

import "fmt"

// checkoutSnapshot selects the immutable snapshot under a checkout's writable upper.
func checkoutSnapshot(h checkoutMeta) string {
	if h.Head != nil {
		return *h.Head
	}
	if h.Parent != nil {
		return *h.Parent
	}
	return ""
}

// retainedLineageLocked finds the ancestry needed by live checkouts and retained snapshots.
func (r *Registry) retainedLineageLocked() map[string]*snapRecord {
	retained := make(map[string]*snapRecord, len(r.lineage))
	visit := func(id string) {
		for id != "" && retained[id] == nil {
			s := r.lineage[id]
			if s == nil {
				break
			}
			retained[id] = s
			id = s.stackParent
		}
	}
	for id := range r.snapshots {
		visit(id)
	}
	for _, h := range r.checkouts {
		visit(checkoutSnapshot(h.meta))
	}
	return retained
}

// pruneLineageLocked releases unneeded dropped ancestry only after a successful state commit.
func (r *Registry) pruneLineageLocked() {
	r.lineage = r.retainedLineageLocked()
}

// restoreStacks rebuilds immutable in-memory stacks and rejects broken or cyclic ancestry.
func restoreStacks(d *diskState) error {
	byID := make(map[string]*diskSnapshot, len(d.Snapshots))
	layers := make(map[string]diskLayer, len(d.Layers))
	for i := range d.Snapshots {
		s := &d.Snapshots[i]
		if byID[s.ID] != nil {
			return fmt.Errorf("duplicate snapshot %s", s.ID)
		}
		byID[s.ID] = s
	}
	for _, layer := range d.Layers {
		layers[layer.ID] = layer
	}
	visited := make(map[string]uint8, len(byID))
	var restore func(string) ([]string, string, error)
	restore = func(id string) ([]string, string, error) {
		s := byID[id]
		if s == nil {
			return nil, "", fmt.Errorf("missing snapshot %s", id)
		}
		if visited[id] == 1 {
			return nil, "", fmt.Errorf("snapshot ancestry cycle at %s", id)
		}
		if visited[id] == 2 {
			return s.Stack, s.Base, nil
		}
		visited[id] = 1
		stack := s.Stack
		if stack != nil {
			for _, id := range stack {
				l, ok := layers[id]
				if !ok || (l.Base != s.Base && !l.Portable) {
					return nil, "", fmt.Errorf("snapshot %s references missing or incompatible layer %s", s.ID, id)
				}
			}
			if s.StackParent != "" {
				if _, _, err := restore(s.StackParent); err != nil {
					return nil, "", err
				}
			}
			visited[id] = 2
			return stack, s.Base, nil
		}
		stack = []string{}
		if s.StackParent != "" {
			parent, base, err := restore(s.StackParent)
			if err != nil {
				return nil, "", err
			}
			if base != s.Base {
				return nil, "", fmt.Errorf("snapshot %s crosses bases", id)
			}
			stack = parent
		}
		if s.Layer != "" {
			if l, ok := layers[s.Layer]; !ok || (l.Base != s.Base && !l.Portable) {
				return nil, "", fmt.Errorf("snapshot %s references missing or incompatible layer %s", id, s.Layer)
			}
			stack = append([]string{s.Layer}, stack...)
		}
		s.Stack = stack
		visited[id] = 2
		return stack, s.Base, nil
	}
	for id := range byID {
		if _, _, err := restore(id); err != nil {
			return err
		}
	}
	for i := range d.Checkouts {
		h := &d.Checkouts[i]
		if h.Stack != nil {
			for _, id := range h.Stack {
				l, ok := layers[id]
				if !ok || (l.Base != h.Base && !l.Portable) {
					return fmt.Errorf("checkout %s references missing or incompatible layer %s", h.Name, id)
				}
			}
			continue
		}
		if id := checkoutSnapshot(h.checkoutMeta); id != "" {
			stack, base, err := restore(id)
			if err != nil {
				return err
			}
			if base != h.Base {
				return fmt.Errorf("checkout %s crosses bases", h.Name)
			}
			h.Stack = stack
		}
	}
	return nil
}
