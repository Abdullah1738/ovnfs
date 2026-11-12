package export

// Export shares persistent file identities between the NFS and SMB protocols.
type Export struct {
	fs     *nodeFS
	volume string
	rt     *Router
}

// New opens the persistent node log and wraps the checkout router.
func New(rt *Router, nodesLog, upperVolume string) (*Export, error) {
	return NewWithLoading(rt, nodesLog, upperVolume, nil)
}

// NewWithLoading reports bytes replayed from the identity log independently of
// registry restoration; loading clients can distinguish progress from a wedge.
func NewWithLoading(rt *Router, nodesLog, upperVolume string, loading func(int, int)) (*Export, error) {
	fs := &nodeFS{Filesystem: rt, Change: rt}
	nodes, err := openNodeTableWithLoading(nodesLog, rt.ReadDir, loading)
	if err != nil {
		return nil, err
	}
	fs.nodes = nodes
	return &Export{fs: fs, volume: upperVolume, rt: rt}, nil
}

// Retire invalidates a checkout's directory and every descendant handle,
// recalling clients' cached copies first while the handles still resolve.
func (e *Export) Retire(name string) {
	e.fs.recallTree(name)
	if e.rt != nil {
		e.rt.retire(name)
	}
	e.fs.nodes.remove(name)
}

// Close flushes outstanding identity records and closes the log.
func (e *Export) Close() error { return e.fs.nodes.close() }
