package export

// Export shares persistent file identities between the NFS and SMB protocols.
type Export struct {
	fs     *nodeFS
	volume string
	rt     *Router
}

// New opens the persistent node log and wraps the checkout router.
func New(rt *Router, nodesLog, upperVolume string) (*Export, error) {
	fs := &nodeFS{Filesystem: rt, Change: rt}
	nodes, err := openNodeTable(nodesLog, rt.ReadDir)
	if err != nil {
		return nil, err
	}
	fs.nodes = nodes
	return &Export{fs: fs, volume: upperVolume, rt: rt}, nil
}

// Retire invalidates a checkout's directory and every descendant handle,
// recalling clients' cached copies first while the handles still resolve.
func (e *Export) Retire(name string) {
	if fn := e.fs.onRecall.Load(); fn != nil {
		for _, t := range e.fs.nodes.subtree(name) {
			(*fn)(t.id)
		}
	}
	if e.rt != nil {
		e.rt.retire(name)
	}
	e.fs.nodes.remove(name)
}

// Close flushes outstanding identity records and closes the log.
func (e *Export) Close() error { return e.fs.nodes.close() }
